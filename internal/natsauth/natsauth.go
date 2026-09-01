// Package natsauth implements Auth V2 Phase 3: decentralized NATS auth via
// per-Account JWTs (the "NSC model" — see docs/AUTH-V2.md §Phase 3).
//
// At runtime ppz-server holds a single Account signing key. On
// /auth/exchange it generates a fresh user nkey pair, mints a User JWT
// with subject permissions scoped to the caller's org, and hands
// (jwt, seed) back to the daemon. NATS validates the chain locally —
// no callback to ppz-server in the data path.
package natsauth

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

// Subjects describes the NATS subject scope embedded in a User JWT.
type Subjects struct {
	Publish   []string
	Subscribe []string
	Deny      []string
}

// SubjectsForOrgUser returns the standard ppz subject scope for a user
// in accountID. The org_id is the lowercase-hyphenated UUID, used as the
// subject root (matches natsubj.SubjectFor).
//
//   pub: <accountID>.>      — own data-plane subjects
//        _INBOX.>       — reply subjects for NATS request/reply
//        $JS.API.>      — JetStream API (stream/consumer mgmt)
//   sub: <accountID>.>      — own data-plane subjects
//        _INBOX.>       — receive replies
//
// Known limitation: $JS.API.> isn't subject-pattern scopable per-org
// (stream names occupy a single subject token, so wildcards can't
// constrain the prefix). All orgs share the same JetStream API
// surface. Data-plane isolation (the load-bearing case for
// broadcasts) IS enforced via the <accountID>.> scope. Per-org account
// isolation is the proper fix; tracked in V3+ out-of-scope.
func SubjectsForOrgUser(accountID string) Subjects {
	return Subjects{
		Publish: []string{
			accountID + ".>", // own data-plane subjects
			"_INBOX.>",   // reply-subjects for request/reply
			"$JS.API.>",  // JetStream API (stream/consumer mgmt)
			"$JS.ACK.>",  // consumer message acknowledgements
		},
		Subscribe: []string{
			accountID + ".>",
			"_INBOX.>",
		},
	}
}

// Account is the runtime state ppz-server needs to mint User JWTs.
// Loaded once at boot from environment via LoadAccountFromEnv.
type Account struct {
	AccountPub string         // public nkey of the Account
	SigningKey nkeys.KeyPair  // derived from PPZ_NATS_ACCOUNT_SIGNING_SEED; held in memory only
	AccountJWT string         // signed by Operator; loaded into the embedded NATS resolver at boot
}

// LoadAccountFromEnv reads the Account credentials from process env
// vars (PPZ_NATS_ACCOUNT_JWT, PPZ_NATS_ACCOUNT_SIGNING_SEED). Fails
// fast at boot if either is missing or malformed.
func LoadAccountFromEnv() (*Account, error) {
	accJWT := os.Getenv("PPZ_NATS_ACCOUNT_JWT")
	if accJWT == "" {
		return nil, errors.New("PPZ_NATS_ACCOUNT_JWT is required")
	}
	signSeed := os.Getenv("PPZ_NATS_ACCOUNT_SIGNING_SEED")
	if signSeed == "" {
		return nil, errors.New("PPZ_NATS_ACCOUNT_SIGNING_SEED is required")
	}
	signKP, err := nkeys.FromSeed([]byte(signSeed))
	if err != nil {
		return nil, fmt.Errorf("parse PPZ_NATS_ACCOUNT_SIGNING_SEED: %w", err)
	}
	claims, err := jwt.DecodeAccountClaims(accJWT)
	if err != nil {
		return nil, fmt.Errorf("decode PPZ_NATS_ACCOUNT_JWT: %w", err)
	}
	return &Account{
		AccountPub: claims.Subject,
		SigningKey: signKP,
		AccountJWT: accJWT,
	}, nil
}

// MintServerUserJWT mints a User JWT with broad pub/sub permissions
// in this Account. ppz-server uses this to authenticate its own
// in-process NATS connection (which manages JetStream streams across
// every org) — no cross-org isolation needed.
func (a *Account) MintServerUserJWT(expiry time.Duration) (jwtStr, seed string, err error) {
	return a.mintUser("ppz-server", []string{">"}, []string{">"}, expiry)
}

// MintUserJWT generates a fresh user nkey pair, signs a short-lived
// User JWT (with the subject scope for accountID embedded), and returns
// (jwt, seed) for handing to a daemon over /auth/exchange. The seed
// is the user's own private key — ppz-server keeps no copy.
func (a *Account) MintUserJWT(accountID string, expiry time.Duration) (jwtStr, seed string, err error) {
	subj := SubjectsForOrgUser(accountID)
	return a.mintUser("ppz-user-"+accountID, subj.Publish, subj.Subscribe, expiry)
}

func (a *Account) mintUser(name string, pubAllow, subAllow []string, expiry time.Duration) (jwtStr, seed string, err error) {
	if a == nil || a.SigningKey == nil {
		return "", "", errors.New("Account.mintUser: nil receiver or unset SigningKey")
	}
	userKP, err := nkeys.CreateUser()
	if err != nil {
		return "", "", fmt.Errorf("create user nkey: %w", err)
	}
	userPub, err := userKP.PublicKey()
	if err != nil {
		return "", "", fmt.Errorf("user public key: %w", err)
	}
	userSeedBytes, err := userKP.Seed()
	if err != nil {
		return "", "", fmt.Errorf("user seed: %w", err)
	}

	claims := jwt.NewUserClaims(userPub)
	claims.Name = name
	// IssuerAccount is required when the JWT is signed by an Account
	// signing key (vs. the Account's primary key) — tells the server
	// to look up the Account by this pub, not the issuer pub.
	claims.IssuerAccount = a.AccountPub
	claims.Pub.Allow = pubAllow
	claims.Sub.Allow = subAllow

	now := time.Now()
	claims.IssuedAt = now.Unix()
	claims.Expires = now.Add(expiry).Unix()
	claims.NotBefore = now.Add(-30 * time.Second).Unix()

	jwtStr, err = claims.Encode(a.SigningKey)
	if err != nil {
		return "", "", fmt.Errorf("encode user jwt: %w", err)
	}
	return jwtStr, string(userSeedBytes), nil
}

// EmbeddedConfig groups the config for StartEmbeddedNATSWithAuth.
// All JWT fields are required; SystemAccountJWT enables JetStream.
type EmbeddedConfig struct {
	Host             string // "" → 127.0.0.1
	Port             int    // 0 → OS picks
	OperatorJWT      string
	AccountJWT       string
	SystemAccountJWT string // optional — required for JetStream
	JetStream        bool   // false → JS disabled (auth-only mode, used by tests)

	// StoreDir is the persistent on-disk path for JetStream FileStorage.
	// Empty falls back to a per-process os.MkdirTemp — fine for tests,
	// loses every stream on restart in any long-lived deployment.
	StoreDir string

	// MaxControlLine bounds the NATS protocol control line — the line
	// that carries the User JWT inside CONNECT. 0 → DefaultMaxControlLine.
	// Set explicitly only to test the bound or to raise it further; see
	// DefaultMaxControlLine for why the stock 4096 is not survivable.
	MaxControlLine int32
}

// DefaultMaxControlLine is the control-line budget the embedded server
// runs with when EmbeddedConfig leaves it unset.
//
// nats-server defaults to 4096. That was survivable while a user JWT
// carried a single per-account wildcard, but ACL Phase 3
// (internal/acl.Compile) emits one subject per pipe per handle, and the
// JWT rides inside the CONNECT control line. Production crossed 4096 at
// four handles (a 4493-byte credential) and every connect began failing
// in the protocol parser, before authentication — reported to users as
// E_NATS_UNREACHABLE with advice to re-login, which cannot help because
// the freshly minted credential is the same size.
//
// 32 KiB is ~7x the credential that broke production and leaves room
// for roughly 30 more handles at the observed ~880 bytes each. The cost
// is per-connection read-buffer headroom, not steady-state allocation.
//
// This raises the ceiling; it does not remove it. The credential still
// grows without bound as an org grows — see docs/AUTH-V2.md §Phase 3.5
// on per-account isolation, which is the fix that ends the class.
const DefaultMaxControlLine = 32768

// StartEmbeddedNATSWithAuth boots an embedded nats-server with
// decentralized auth: the supplied operatorJWT is set as the trusted
// operator, and the account JWTs are preloaded into the in-memory
// resolver. JetStream + auth requires a system account.
//
// Returns the running server. Caller is responsible for Shutdown().
func StartEmbeddedNATSWithAuth(cfg EmbeddedConfig) (*natsserver.Server, error) {
	opClaims, err := jwt.DecodeOperatorClaims(cfg.OperatorJWT)
	if err != nil {
		return nil, fmt.Errorf("decode operator jwt: %w", err)
	}

	// Build the resolver and pre-load any account JWTs *before*
	// NewServer is called — the server resolves SystemAccount during
	// construction and fails fast if it can't find it.
	resolver := &natsserver.MemAccResolver{}
	if cfg.AccountJWT != "" {
		accClaims, err := jwt.DecodeAccountClaims(cfg.AccountJWT)
		if err != nil {
			return nil, fmt.Errorf("decode account jwt: %w", err)
		}
		if err := resolver.Store(accClaims.Subject, cfg.AccountJWT); err != nil {
			return nil, fmt.Errorf("store account: %w", err)
		}
	}

	var sysAccClaims *jwt.AccountClaims
	if cfg.SystemAccountJWT != "" {
		sysAccClaims, err = jwt.DecodeAccountClaims(cfg.SystemAccountJWT)
		if err != nil {
			return nil, fmt.Errorf("decode system account jwt: %w", err)
		}
		if err := resolver.Store(sysAccClaims.Subject, cfg.SystemAccountJWT); err != nil {
			return nil, fmt.Errorf("store system account: %w", err)
		}
	}

	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	maxControlLine := cfg.MaxControlLine
	if maxControlLine == 0 {
		maxControlLine = DefaultMaxControlLine
	}
	opts := &natsserver.Options{
		Host:             host,
		Port:             cfg.Port,
		TrustedOperators: []*jwt.OperatorClaims{opClaims},
		AccountResolver:  resolver,
		// The User JWT travels inside the CONNECT control line, and ACL
		// Phase 3 credentials scale with the org. See
		// DefaultMaxControlLine.
		MaxControlLine: maxControlLine,
		// Embedded: the host process owns signal handling. Without this,
		// nats-server's Start() installs its own SIGINT/SIGTERM handler
		// whose Shutdown races ours and panics (close of nil channel in
		// shutdownEventing), killing the process before deferred cleanup
		// runs.
		NoSigs: true,
	}
	if sysAccClaims != nil {
		opts.SystemAccount = sysAccClaims.Subject
	}

	if cfg.JetStream {
		if sysAccClaims == nil {
			return nil, errors.New("StartEmbeddedNATSWithAuth: JetStream requires SystemAccountJWT")
		}
		storeDir := cfg.StoreDir
		if storeDir == "" {
			storeDir, err = os.MkdirTemp("", "ppz-jetstream-")
			if err != nil {
				return nil, fmt.Errorf("mkdir jetstream store: %w", err)
			}
		} else if err := os.MkdirAll(storeDir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir jetstream store %s: %w", storeDir, err)
		}
		opts.JetStream = true
		opts.StoreDir = storeDir
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("nats new: %w", err)
	}
	go ns.Start()
	return ns, nil
}
