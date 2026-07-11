package server

import (
	"strings"
	"testing"
)

func TestOAuthState_MintVerifyRoundTrip(t *testing.T) {
	tok, err := MintOAuthState(testSessionKey, "/dashboard")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}
	st, err := VerifyOAuthState(testSessionKey, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if st.Next != "/dashboard" {
		t.Errorf("Next: got %q want %q", st.Next, "/dashboard")
	}
	if st.Nonce == "" {
		t.Error("Nonce must be populated on a verified state")
	}
}

func TestOAuthState_TamperedRejected(t *testing.T) {
	tok, err := MintOAuthState(testSessionKey, "/dashboard")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// See session_test.go's TestSessionCookie_TamperedRejected for why
	// we tamper the first char of the signature, not the last.
	dot := strings.IndexByte(tok, '.')
	if dot < 0 || dot+1 >= len(tok) {
		t.Fatalf("malformed token %q: no signature half", tok)
	}
	flip := byte('A')
	if tok[dot+1] == 'A' {
		flip = 'B'
	}
	tampered := tok[:dot+1] + string(flip) + tok[dot+2:]
	if _, err := VerifyOAuthState(testSessionKey, tampered); err == nil {
		t.Fatal("verify must reject tampered token")
	}
}

func TestOAuthState_ReplayRejected(t *testing.T) {
	tok, err := MintOAuthState(testSessionKey, "/dashboard")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyOAuthState(testSessionKey, tok); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := VerifyOAuthState(testSessionKey, tok); err == nil {
		t.Fatal("verify must reject replayed token (nonce already consumed)")
	}
}

func TestOAuthState_WrongKeyRejected(t *testing.T) {
	tok, err := MintOAuthState(testSessionKey, "/dashboard")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	other := SessionKey([]byte("different-key-aaaaaaaaaaaaaaaaaaaa"))
	if _, err := VerifyOAuthState(other, tok); err == nil {
		t.Fatal("verify must reject token signed with a different key")
	}
}

func TestOAuthState_NextPathSafe(t *testing.T) {
	// MintOAuthState should reject obviously dangerous `next` values
	// (open-redirect to an external host, or schemes other than path).
	cases := []string{
		"https://evil.example.com/",
		"//evil.example.com/",
		"javascript:alert(1)",
		`/\evil.example.com`, // backslash: several browsers normalize \ -> / (protocol-relative)
	}
	for _, c := range cases {
		if _, err := MintOAuthState(testSessionKey, c); err == nil {
			t.Errorf("mint must reject unsafe next=%q", c)
		}
	}
}
