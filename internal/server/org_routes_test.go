package server

import (
	"os"
	"regexp"
	"testing"
)

// Every route under /orgs/{id} must be registered through orgSession,
// which enforces org membership. requireSession alone only proves the
// caller is logged in — a route registered that way is readable (or
// writable) by any user of any org. Scans http.go because ServeMux
// doesn't expose its registered handlers.
func TestOrgRoutesUseOrgSession(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	route := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ /orgs/\{id\}[^"]*)",\s*(.*)\)`)
	matches := route.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no /orgs/{id} routes found — has the registration syntax changed?")
	}
	for _, m := range matches {
		if !regexp.MustCompile(`^s\.orgSession\(`).MatchString(m[2]) {
			t.Errorf("%s is registered with %s; wrap it in s.orgSession", m[1], m[2])
		}
	}
}
