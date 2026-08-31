package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// withAuthedCaller returns a context carrying an AuthedCaller with the
// given user id — drives handler-level guards in unit tests without
// spinning up the bearer middleware against a real DB.
func withAuthedCaller(ctx context.Context, uid uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyAuthedCaller, AuthedCaller{UserID: uid})
}

// uuidNonZero returns a stable nonzero uuid for tests that just need
// "any user id" — handlers that look it up against the DB will fail
// further down, but pure validation paths don't get that far.
func uuidNonZero() uuid.UUID {
	return uuid.MustParse("11111111-2222-3333-4444-555555555555")
}

// These cover the non-DB paths of the invite handlers — auth gates,
// JSON-shape validation, path-value parsing. Behavioural coverage
// (the actual create/list/accept flow against a real DB) lives in the
// e2e bash suite under tests/invite/*.

func TestInvites_API_NoAuth_401(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               string
	}{
		{"createInvite", "POST", "/api/v1/orgs/alpha/invites", `{"username":"alice"}`},
		{"listOrgInvites", "GET", "/api/v1/orgs/alpha/invites", ""},
		{"revokeInvite", "POST", "/api/v1/orgs/alpha/invites/00000000-0000-0000-0000-000000000000/revoke", ""},
		{"listMine", "GET", "/api/v1/invites", ""},
		{"accept", "POST", "/api/v1/invites/00000000-0000-0000-0000-000000000000/accept", ""},
		{"decline", "POST", "/api/v1/invites/00000000-0000-0000-0000-000000000000/decline", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			}
			var req *http.Request
			if body != nil {
				req = httptest.NewRequest(tc.method, tc.path, body)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status: got %d want 401 (no Authorization header)", rec.Code)
			}
		})
	}
}

func TestAPICreateInvite_MissingUsername_400(t *testing.T) {
	// Drive handler directly with a context that contains a
	// "valid-looking" OAuth caller (nonzero UserID). Handler should
	// reject empty username before any DB call.
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orgs/alpha/invites", bytes.NewReader([]byte(`{"username":""}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("slug", "alpha")

	ctx := withAuthedCaller(req.Context(), uuidNonZero())
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAPICreateInvite(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty username should 400; got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPICreateInvite_NoPrincipal_403(t *testing.T) {
	// A request with no authenticated caller has UserID = uuid.Nil →
	// handler should 403 without touching the DB.
	//
	// Renamed in ACL Phase 0a: this used to be called
	// ..._APIKeyCaller_403 on the premise that API-key bearers also
	// carry uuid.Nil. They no longer do — a key resolves to its
	// principal — so the only caller this guard rejects is an
	// unauthenticated one.
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orgs/alpha/invites", bytes.NewReader([]byte(`{"username":"alice"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("slug", "alpha")
	// No authed caller in context at all.
	rec := httptest.NewRecorder()
	srv.handleAPICreateInvite(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("caller with no principal should 403; got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIAcceptInvite_BadInviteID_400(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invites/not-a-uuid/accept", nil)
	req.SetPathValue("id", "not-a-uuid")

	ctx := withAuthedCaller(req.Context(), uuidNonZero())
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAPIAcceptInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid invite uuid should 400; got %d", rec.Code)
	}
}

func TestAPIDeclineInvite_BadInviteID_400(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invites/not-a-uuid/decline", nil)
	req.SetPathValue("id", "not-a-uuid")

	ctx := withAuthedCaller(req.Context(), uuidNonZero())
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAPIDeclineInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid invite uuid should 400; got %d", rec.Code)
	}
}

func TestAPIRevokeInvite_BadInviteID_400(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orgs/alpha/invites/not-a-uuid/revoke", nil)
	req.SetPathValue("slug", "alpha")
	req.SetPathValue("id", "not-a-uuid")

	ctx := withAuthedCaller(req.Context(), uuidNonZero())
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAPIRevokeInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid invite uuid should 400; got %d", rec.Code)
	}
}
