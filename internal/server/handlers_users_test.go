package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// POST /users creates arbitrary user rows, so outside DevLogin (i.e. in
// production, where users arrive via OAuth) it must 404 before touching
// the database. Server has no Pool here: reaching the insert would panic.
func TestCreateUserRequiresDevLogin(t *testing.T) {
	srv := &Server{}
	form := url.Values{"username": {"mallory"}, "email": {"m@example.com"}, "mode": {"internal"}}
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleCreateUser(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Outside DevLogin, POST /orgs must refuse owner_user_id rather than
// silently ignore it: honouring it would let anyone create orgs owned by
// someone else, and ignoring it would hide that from the caller.
func TestCreateOrgRejectsOwnerOverrideOutsideDevLogin(t *testing.T) {
	srv := &Server{}
	form := url.Values{"name": {"wonder"}, "owner_user_id": {"00000000-0000-0000-0000-000000000001"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleGUICreateOrg(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
