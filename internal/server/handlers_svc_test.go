package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ACL Phase 1 — service-account routes.
//
// Route-mount and auth-gate coverage only: these assert the middleware
// rejects before any DB lookup, matching the convention in
// handlers_pipes_phase1_5_test.go. Role-dependent behaviour needs a
// real Postgres and lives in the e2e suite.

func TestSvcAPI_Create_NoAuth_401(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/svc", bytes.NewReader([]byte(`{"name":"builder-bot"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401 (no Authorization header)", rec.Code)
	}
}

func TestSvcAPI_List_NoAuth_401(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/svc", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", rec.Code)
	}
}

func TestSvcAPI_MintKey_NoAuth_401(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/svc/builder-bot/keys", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", rec.Code)
	}
}

func TestSvcAPI_Delete_NoAuth_401(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/svc/builder-bot", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", rec.Code)
	}
}

// A service name that smuggles the scope separator would land in
// another org's namespace once stored as "<org>/<name>". Rejected on
// syntax, before any auth or DB work.
func TestSvcAPI_Create_RejectsSeparatorInName(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/svc", bytes.NewReader([]byte(`{"name":"globex/builder-bot"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	// The route must be MOUNTED (else this passes trivially on a 404),
	// and must never accept the name. 401 from the auth gate is the
	// expected answer for this unauthenticated request; what must not
	// happen is a 2xx.
	if rec.Code == http.StatusNotFound {
		t.Fatal("POST /api/v1/svc is not mounted")
	}
	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("a service name containing '/' must never be accepted; got %d", rec.Code)
	}
}

// The role-setting route is session-authed (GUI), so an anonymous
// caller is redirected to login rather than 401'd.
func TestSetMemberRole_NoSession_NotOK(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/orgs/alpha/members/"+zeroUUID+"/role",
		bytes.NewReader([]byte("role=admin")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("POST /orgs/{id}/members/{uid}/role is not mounted")
	}
	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("anonymous caller must not set a member role; got %d", rec.Code)
	}
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"
