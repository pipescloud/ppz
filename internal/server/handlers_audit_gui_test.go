package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The audit trail surfaces as a fifth ORG tab, not a new global admin
// section. The GUI has no global admin surface today — "admin" is two
// test-only API endpoints that 404 in prod — and audit data is
// per-account anyway, so /orgs/{id}/audit reuses the existing tab
// router, session gate and org resolution rather than inventing a
// parallel hierarchy.

func TestOrgTabs_IncludesAudit(t *testing.T) {
	if !orgTabs["audit"] {
		t.Error(`orgTabs["audit"] is false — GET /orgs/{id}/audit would 404 as an unknown tab`)
	}
}

// Session gate: the tab is not public. requireSession redirects an
// anonymous browser to the login page rather than rendering.
func TestAuditTab_AnonymousIsNotServed(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("GET", "/orgs/alpha/audit", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("anonymous GET /orgs/{id}/audit returned 200 — the tab must sit behind requireSession")
	}
}

// The nav has to carry the link or the tab is unreachable by clicking,
// which is the only way anyone will find it.
func TestOrgTemplate_NavHasAuditTab(t *testing.T) {
	data, err := templateFS.ReadFile("templates/org.html")
	if err != nil {
		t.Fatalf("read org.html: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `data-tab="audit"`) {
		t.Error(`org.html nav has no data-tab="audit" link`)
	}
	if !strings.Contains(body, `{{if eq .ActiveTab "audit"}}`) {
		t.Error(`org.html has no {{if eq .ActiveTab "audit"}} section to render the trail`)
	}
}

// Rows need machine-readable hooks for the same reason the keys table
// has data-key-id / data-key-state: the e2e scenarios assert against
// them instead of scraping rendered prose.
func TestOrgTemplate_AuditRowsCarryDataAttributes(t *testing.T) {
	data, err := templateFS.ReadFile("templates/org.html")
	if err != nil {
		t.Fatalf("read org.html: %v", err)
	}
	body := string(data)
	for _, attr := range []string{"data-audit-action=", "data-audit-target=", "data-audit-actor="} {
		if !strings.Contains(body, attr) {
			t.Errorf("org.html audit rows missing %s", attr)
		}
	}
}
