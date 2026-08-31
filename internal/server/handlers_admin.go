package server

// Test-only admin endpoint. Gated on DevLogin (PPZ_DEV_LOGIN=true) so
// it returns 404 in production. Used by tests/lib/reset.sh between
// scenarios to wipe JetStream state across every per-org account.
//
// Phase 3.5 introduced per-org NATS accounts, which means each org's
// streams live in its own subject namespace + JS instance. The old
// reset path used the `nats` CLI from the test-runner with the
// legacy server-user creds, which only saw streams in the legacy
// shared account. With per-org accounts, that approach loses sight
// of any stream the system has provisioned. ppz-server is the only
// thing that holds connections into every account — so cleanup
// belongs here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/pipescloud/ppz/internal/db"
	"github.com/pipescloud/ppz/internal/natsauth"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// handleAdminWipe deletes every source_*/pipe_* JetStream stream
// across every provisioned org's account. Idempotent. Returns 200
// regardless of how many streams it actually removed (a fresh
// stack with no streams returns 200 just the same).
//
// Route: POST /api/v1/admin/wipe (gated on DevLogin).
func (s *Server) handleAdminWipe(w http.ResponseWriter, r *http.Request) {
	if !s.DevLogin {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	orgs, err := db.ListAccounts(ctx, s.Pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, org := range orgs {
		// Skip orgs whose account hasn't been provisioned yet — there
		// can't be any streams in an unborn account.
		if org.NATSAccountJWT == "" {
			continue
		}
		oa, err := s.AccountPool.Get(ctx, org.ID)
		if err != nil {
			// Best-effort — log via the standard handler chain (just
			// continue; reset.sh treats overall 200 as "good enough").
			continue
		}
		_ = wipeStreams(ctx, oa.JS)
	}
	w.WriteHeader(http.StatusOK)
}

// handleSimulateStaleOperator forces an org's NATS account JWT into
// the same "signed by an operator the running NATS server doesn't
// trust" state that an operator-key rotation produces in production.
// Used by tests/recovery/* to drive the auth-violation recovery path
// in AccountPool without actually rotating the running operator.
//
// The endpoint generates an in-memory FAKE operator + fresh account
// material, signs an account JWT with the fake operator, overwrites
// the org's nats_account_* row, and drops the AccountPool cache.
// Postgres remains self-consistent (matching pub/jwt/seed) but the
// JWT no longer chains to a trusted root — which is exactly the
// state we hit on 2026-05-03 in production.
//
// Route: POST /api/v1/admin/simulate-stale-operator (gated on DevLogin).
//
// Body: {"api_key":"<plaintext>"}
func (s *Server) handleSimulateStaleOperator(w http.ResponseWriter, r *http.Request) {
	if !s.DevLogin {
		http.NotFound(w, r)
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.APIKey == "" {
		http.Error(w, "missing api_key", http.StatusBadRequest)
		return
	}
	ctx, cancel := withTimeout(r)
	defer cancel()

	key, err := db.LookupAPIKey(ctx, s.Pool, req.APIKey)
	if err != nil {
		http.Error(w, "api key lookup: "+err.Error(), http.StatusUnauthorized)
		return
	}
	accountID := key.AccountID

	// Fake operator — never persisted; only used to sign the about-to-
	// be-orphaned account JWT.
	fakeOpKP, err := nkeys.CreateOperator()
	if err != nil {
		http.Error(w, "fake operator: "+err.Error(), http.StatusInternalServerError)
		return
	}
	newAccKP, err := nkeys.CreateAccount()
	if err != nil {
		http.Error(w, "new account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	newSignKP, err := nkeys.CreateAccount()
	if err != nil {
		http.Error(w, "new signing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	newAccPub, _ := newAccKP.PublicKey()
	newSignSeed, _ := newSignKP.Seed()

	fakeAccJWT, err := natsauth.MintAccountJWT(fakeOpKP,
		"ppz-stale-"+accountID.String(), newAccKP, newSignKP)
	if err != nil {
		http.Error(w, "mint fake account jwt: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := db.SetNATSAccount(ctx, s.Pool, accountID,
		newAccPub, fakeAccJWT, string(newSignSeed)); err != nil {
		http.Error(w, "save org account: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.AccountPool.Drop(accountID)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// wipeStreams deletes every ppz-owned stream in this account. Other
// streams (internal $JS machinery, anything a user created directly)
// are left alone.
//
// The prefix set lives in natsubj so a new stream family cannot escape
// the wipe — which is exactly what happened when presence_ was added.
func wipeStreams(ctx context.Context, js jetstream.JetStream) error {
	lister := js.ListStreams(ctx)
	var deleteErrs []string
	for info := range lister.Info() {
		name := info.Config.Name
		if !natsubj.IsPPZStream(name) {
			continue
		}
		if err := js.DeleteStream(ctx, name); err != nil {
			deleteErrs = append(deleteErrs, name+": "+err.Error())
		}
	}
	if err := lister.Err(); err != nil {
		deleteErrs = append(deleteErrs, "list: "+err.Error())
	}
	if len(deleteErrs) > 0 {
		return errors.New("wipe errors: " + strings.Join(deleteErrs, "; "))
	}
	return nil
}
