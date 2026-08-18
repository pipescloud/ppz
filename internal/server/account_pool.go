package server

// Auth V2 §Phase 3.5 — per-org NATS account pool. Each org gets its
// own NATS account, lazily provisioned on first use. ppz-server
// holds one in-process nats.Conn per active account (authenticated
// as the account's "server user" with broad perms in that account).
// Handlers that need to publish/subscribe/manage JetStream streams
// for an org route through the pool's per-org connection rather
// than a single shared one.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsauth"
)

// OrgAccount is the runtime state ppz-server holds for one
// provisioned org's NATS account. Closed when the org is deleted.
type OrgAccount struct {
	AccountID      uuid.UUID
	AccountName    string
	AccountPub string

	// SigningKP is loaded from the per-org signing seed on first
	// access; used to mint per-(user, org) JWTs in /auth/exchange.
	SigningKP nkeys.KeyPair

	// NC is the in-process server-user connection — broad pub/sub
	// in this account. Used by handlers for JetStream management
	// and by the broadcast-subscriber.
	NC *nats.Conn
	JS jetstream.JetStream

	cleanup func()
}

// attachCleanup chains an additional cleanup function to run when
// the OrgAccount is closed. Used by per-account subscribers to
// hook in their Unsubscribe.
func (oa *OrgAccount) attachCleanup(fn func()) {
	prev := oa.cleanup
	oa.cleanup = func() {
		fn()
		if prev != nil {
			prev()
		}
	}
}

// AccountPool is the per-org NATS account registry. Lazy: orgs are
// provisioned on first lookup, kept warm for the server's lifetime
// or until explicitly closed (org deletion).
type AccountPool struct {
	mu     sync.Mutex
	byOrg  map[uuid.UUID]*OrgAccount
	server *Server // for DB pool + resolver + operator key
}

func newAccountPool(s *Server) *AccountPool {
	return &AccountPool{
		byOrg:  make(map[uuid.UUID]*OrgAccount),
		server: s,
	}
}

// Get returns the OrgAccount for accountID, provisioning it if needed.
// Provisioning steps (one-time per org):
//
//  1. Generate fresh Account + signing keypairs
//  2. MintAccountJWT signed by the Operator
//  3. Persist (account_pub, account_jwt, signing_seed) on the org row
//  4. Register the Account JWT with the live NATS resolver
//  5. Mint a server-user JWT in the new account (broad perms)
//  6. Open an in-process nats.Conn authenticated as that user
//  7. Construct a JetStream client + cache the OrgAccount
//
// Concurrent calls for the same org coalesce on the mutex; one
// goroutine provisions, the rest reuse the cached entry.
func (p *AccountPool) Get(ctx context.Context, accountID uuid.UUID) (*OrgAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if oa, ok := p.byOrg[accountID]; ok {
		return oa, nil
	}

	org, err := db.GetAccount(ctx, p.server.Pool, accountID)
	if err != nil {
		return nil, fmt.Errorf("org lookup: %w", err)
	}

	provisioned := false
	// Provision-on-first-touch if the row hasn't been signed yet.
	if org.NATSAccountJWT == "" {
		if err := p.provisionAccount(ctx, &org); err != nil {
			return nil, fmt.Errorf("provision %s: %w", org.Name, err)
		}
		provisioned = true
	}

	oa, err := p.openAccount(org)
	if err != nil && isAuthViolation(err) && !provisioned {
		// Cached account JWT is no longer trusted by the running NATS
		// server — most commonly because the operator key was rotated
		// (intentionally or via infra-deploy churn) since the JWT was
		// minted. Reprovision: mint fresh material signed by the
		// CURRENT operator + push to resolver, then retry openAccount.
		// ensureStreamsForOrg below picks up the new account namespace
		// and recreates streams for any DB-tracked sources/pipes.
		log.Printf("account_pool: auth violation opening %s, reprovisioning: %v", org.Name, err)
		if perr := p.provisionAccount(ctx, &org); perr != nil {
			return nil, fmt.Errorf("reprovision %s after auth violation: %w", org.Name, perr)
		}
		provisioned = true
		oa, err = p.openAccount(org)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", org.Name, err)
	}

	// Lazy-create JetStream streams for the org's existing sources and
	// user pipes. Idempotent: existing streams → no-op. This is what
	// closes the "DB has source rows / NATS has empty account" gap that
	// shows up post-recovery (and would also show up after a JS storage
	// loss, e.g. EC2 root-volume replacement).
	if err := p.ensureStreamsForOrg(ctx, oa); err != nil {
		// Best-effort. If a particular stream fails, the daemon's
		// pre-publish stream check will surface it as E_INVALID_PIPE
		// on the affected pipe — recovery is partial, not silent.
		log.Printf("account_pool: ensure-streams for %s: %v", org.Name, err)
	}

	p.byOrg[accountID] = oa
	return oa, nil
}

// Drop evicts the cached OrgAccount for accountID, closing the underlying
// NATS connection. Used by the dev-gated /api/v1/admin/simulate-stale-
// operator endpoint and (potentially) by future operations like
// org deletion.
func (p *AccountPool) Drop(accountID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if oa, ok := p.byOrg[accountID]; ok {
		if oa.cleanup != nil {
			oa.cleanup()
		}
		if oa.NC != nil {
			oa.NC.Close()
		}
		delete(p.byOrg, accountID)
	}
}

// isAuthViolation detects the NATS-server-side rejection we see when an
// account JWT in postgres no longer chains to a trusted operator. The
// nats.go client surfaces this as an *nats.Error with the literal
// "Authorization Violation" message; we string-match because the typed
// constant isn't exported.
func isAuthViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Authorization Violation") ||
		strings.Contains(msg, "authorization violation")
}

// ensureStreamsForOrg creates any missing JetStream streams for the
// org's existing sources + user pipes, in the currently-active account
// namespace. Idempotent — `ensurePipeStream` swallows
// ErrStreamNameAlreadyInUse so repeat calls are cheap when streams
// already exist.
//
// Called from Get after the account has been (re-)opened. Together with
// the auth-violation retry above, this is what makes account
// reprovisioning self-healing for sources/pipes — the system rebuilds
// the JetStream-side cache from postgres on demand.
func (p *AccountPool) ensureStreamsForOrg(ctx context.Context, oa *OrgAccount) error {
	sources, err := db.ListSourcesForOrg(ctx, p.server.Pool, oa.AccountID)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	for _, src := range sources {
		// Auto-provisioned pipes (kind-derived: broadcast / stdin /
		// stdout / stdctrl).
		for _, pipe := range src.Pipes() {
			if err := ensurePipeStream(ctx, oa.JS, oa.AccountID, src.Manifold, src.Handle, pipe); err != nil {
				return fmt.Errorf("ensure auto stream %s.%s: %w", src.Handle, pipe, err)
			}
		}
		// User-created pipes (from the `pipes` table, with stored
		// retention overrides).
		userPipes, err := db.ListPipesForSource(ctx, p.server.Pool, src.ID)
		if err != nil {
			return fmt.Errorf("list pipes for %s: %w", src.Handle, err)
		}
		for _, up := range userPipes {
			age, msgs, bytes := pipeRetention(up)
			if err := ensurePipeStreamWithRetention(ctx, oa.JS, oa.AccountID, up.Manifold, src.Handle, up.Name, age, msgs, bytes); err != nil {
				return fmt.Errorf("ensure user stream %s.%s: %w", src.Handle, up.Name, err)
			}
		}
	}
	return nil
}

// pipeRetention resolves a Pipe row's stored overrides into concrete
// (maxAge, maxMsgs, maxBytes) values. A thin wrapper over
// resolveRetention so this path and the HTTP handlers can't disagree
// about precedence.
func pipeRetention(pipe db.Pipe) (time.Duration, int, int64) {
	return resolveRetention(pipeLayer(pipe))
}

// pipeLayer lifts a stored row into the highest-precedence retention
// layer.
func pipeLayer(pipe db.Pipe) retentionOverride {
	return retentionOverride{
		TTLSeconds: pipe.TTLSeconds,
		MaxMsgs:    pipe.MaxMsgs,
		MaxBytes:   pipe.MaxBytes,
	}
}

// provisionAccount mints + registers + persists a brand-new account
// for org. Mutates `org` with the freshly-set fields.
func (p *AccountPool) provisionAccount(ctx context.Context, org *db.Account) error {
	if p.server.OperatorKP == nil {
		return fmt.Errorf("server operator key is not loaded")
	}
	accKP, err := nkeys.CreateAccount()
	if err != nil {
		return fmt.Errorf("create account nkey: %w", err)
	}
	signKP, err := nkeys.CreateAccount()
	if err != nil {
		return fmt.Errorf("create signing nkey: %w", err)
	}
	accPub, _ := accKP.PublicKey()
	signSeed, _ := signKP.Seed()

	accJWT, err := natsauth.MintAccountJWT(p.server.OperatorKP,
		"ppz-tenant-"+org.Name, accKP, signKP)
	if err != nil {
		return fmt.Errorf("mint account jwt: %w", err)
	}

	// Persist before publishing to the resolver — if the resolver
	// store fails, we want the row to reflect "not yet active" too.
	if err := db.SetNATSAccount(ctx, p.server.Pool, org.ID, accPub, accJWT, string(signSeed)); err != nil {
		return fmt.Errorf("save org account: %w", err)
	}

	// Register the new account JWT with the live in-memory resolver
	// so subsequent connections under this account succeed.
	if err := p.server.NATSResolver.Store(accPub, accJWT); err != nil {
		return fmt.Errorf("resolver store: %w", err)
	}

	org.NATSAccountPub = accPub
	org.NATSAccountJWT = accJWT
	org.NATSAccountSigningSeed = string(signSeed)
	return nil
}

// openAccount mints a server-user JWT for the org's account and
// opens a connection authenticated as that user. The connection
// has broad pub/sub perms within the account (the account boundary
// is what enforces tenant isolation, not within-account perms).
func (p *AccountPool) openAccount(org db.Account) (*OrgAccount, error) {
	signKP, err := nkeys.FromSeed([]byte(org.NATSAccountSigningSeed))
	if err != nil {
		return nil, fmt.Errorf("parse signing seed: %w", err)
	}

	srvUserExp := time.Now().Add(365 * 24 * time.Hour).Unix()
	srvJWT, srvSeed, err := natsauth.MintUserJWTInAccount(
		org.NATSAccountPub, signKP,
		"ppz-server-"+org.Name,
		[]string{">"}, []string{">"},
		srvUserExp,
	)
	if err != nil {
		return nil, fmt.Errorf("mint server user: %w", err)
	}
	srvKP, err := nkeys.FromSeed([]byte(srvSeed))
	if err != nil {
		return nil, fmt.Errorf("server-user seed parse: %w", err)
	}

	nc, err := nats.Connect(p.server.NATSClientURL,
		nats.UserJWT(
			func() (string, error) { return srvJWT, nil },
			func(nonce []byte) ([]byte, error) { return srvKP.Sign(nonce) },
		),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect (account=%s): %w", org.Name, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream client: %w", err)
	}

	oa := &OrgAccount{
		AccountID:      org.ID,
		AccountName:    org.Name,
		AccountPub: org.NATSAccountPub,
		SigningKP:  signKP,
		NC:         nc,
		JS:         js,
		cleanup:    func() { nc.Close() },
	}

	return oa, nil
}

// Close releases all per-org connections. Called on server shutdown.
func (p *AccountPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, oa := range p.byOrg {
		if oa.cleanup != nil {
			oa.cleanup()
		}
	}
	p.byOrg = nil
}

// JSFor is a thin convenience wrapper handlers use to fetch the
// per-org JetStream client without dragging the full *OrgAccount
// through. Calls AccountPool.Get under the hood, so the org is
// provisioned on first use.
func (s *Server) JSFor(ctx context.Context, accountID uuid.UUID) (jetstream.JetStream, error) {
	oa, err := s.AccountPool.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return oa.JS, nil
}

// NCFor returns the per-org NATS connection — used by handlers that
// need raw pub/sub (e.g. terminal WebSocket subscribers).
func (s *Server) NCFor(ctx context.Context, accountID uuid.UUID) (*nats.Conn, error) {
	oa, err := s.AccountPool.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return oa.NC, nil
}

// Forget removes an org from the pool (e.g. on org deletion). The
// caller is responsible for any pre-close cleanup (draining streams
// etc.). This just closes the connection and clears the cache.
func (p *AccountPool) Forget(accountID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if oa, ok := p.byOrg[accountID]; ok {
		if oa.cleanup != nil {
			oa.cleanup()
		}
		delete(p.byOrg, accountID)
	}
}

// natsResolverFromOpts is a small shim to expose the embedded NATS
// server's account resolver. We need it for runtime account adds.
// (NATS server's `Server.AccountResolver()` returns the same handle
// we passed in via Options.AccountResolver — typing through the
// public API is just a helper.)
func natsResolverFromServer(ns *natsserver.Server) natsserver.AccountResolver {
	return ns.AccountResolver()
}

// silence unused; kept for documentation of where the resolver
// reference originates
var _ = jwt.Version
