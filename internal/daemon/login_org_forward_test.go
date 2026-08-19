package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// TestHandleLogin_ForwardsChosenAccountID — when the user picks an org in
// the OAuth device flow, the CLI hands that org to the daemon as
// LoginRequest.AccountID. handleLogin must forward it as
// AuthExchangeRequest.AccountID so the server mints the NATS JWT in the
// chosen org (the server validates membership there). Dropping it sends
// the caller back to the server's default org — the original bug.
func TestHandleLogin_ForwardsChosenAccountID(t *testing.T) {
	const beta = "00000000-0000-0000-0000-0000000000b2"

	var gotReqAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req cliproto.AuthExchangeRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		gotReqAccountID = req.AccountID
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okExchangeReply(req.AccountID))
	}))
	defer srv.Close()

	d := newLoginTestDaemon(t)
	if _, e := driveLogin(t, d, cliproto.LoginRequest{URL: srv.URL, APIKey: "ppz_oauth_test", AccountID: beta}); e != nil {
		t.Fatalf("login returned error: %v", e)
	}

	if gotReqAccountID != beta {
		t.Fatalf("handleLogin sent AuthExchangeRequest.AccountID=%q, want %q "+
			"(the org chosen in the device flow); dropping it logs the user into the server default org",
			gotReqAccountID, beta)
	}
}
