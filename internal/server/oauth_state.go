package server

// OAuth `state` parameter — the CSRF token we hand to GitHub at
// /auth/github/start and verify when the user comes back through
// /auth/github/callback. The state blob also round-trips an opaque
// `next` URL so the callback can redirect to the page the user was
// originally trying to reach.
//
// Wire format: base64url(payload-json) + "." + base64url(hmac-sha256(payload, key))
// Payload: {"n":"<nonce>","r":"<next-path>","exp":<unix>}
//
// Replay protection: every successful Verify atomically marks the
// nonce as consumed. A second call with the same token fails. The
// nonce store is in-memory; consumed entries expire on a sweep timer
// so we don't grow unbounded.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrInvalidOAuthState = errors.New("invalid oauth state")

// stateTTL bounds how long a freshly-minted state token is valid.
// 10 min is plenty for a user to walk through GitHub's consent screen.
const stateTTL = 10 * time.Minute

// OAuthState is what Verify returns on success.
type OAuthState struct {
	Nonce string
	Next  string
}

type oauthStateWire struct {
	N   string `json:"n"`
	R   string `json:"r"`
	Exp int64  `json:"exp"`
}

// nonceStore tracks which nonces have been consumed. Process-local;
// fine because OAuth is short-lived and confined to a single server.
type nonceStore struct {
	mu       sync.Mutex
	consumed map[string]time.Time
}

func (n *nonceStore) tryConsume(nonce string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.consumed == nil {
		n.consumed = map[string]time.Time{}
	}
	// Lazy GC: every call walks the map and drops stuff older than
	// 2x stateTTL. Cheap because the map only ever holds in-flight
	// flows (worst case: dozens of entries).
	cutoff := time.Now().Add(-2 * stateTTL)
	for k, t := range n.consumed {
		if t.Before(cutoff) {
			delete(n.consumed, k)
		}
	}
	if _, seen := n.consumed[nonce]; seen {
		return false
	}
	n.consumed[nonce] = time.Now()
	return true
}

var defaultNonceStore = &nonceStore{}

// MintOAuthState produces an HMAC-signed token containing a fresh
// random nonce + the desired post-auth `next` path. Rejects unsafe
// `next` values (anything that looks like an open-redirect away
// from this site).
func MintOAuthState(key SessionKey, next string) (string, error) {
	if !isSafeNextPath(next) {
		return "", errors.New("unsafe next path")
	}
	if len(key) == 0 {
		return "", errors.New("session key is empty")
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	body, err := json.Marshal(oauthStateWire{
		N:   base64.RawURLEncoding.EncodeToString(nonceBytes),
		R:   next,
		Exp: time.Now().Add(stateTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyOAuthState(key SessionKey, token string) (OAuthState, error) {
	if token == "" || len(key) == 0 {
		return OAuthState{}, ErrInvalidOAuthState
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return OAuthState{}, ErrInvalidOAuthState
	}
	body, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return OAuthState{}, ErrInvalidOAuthState
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return OAuthState{}, ErrInvalidOAuthState
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return OAuthState{}, ErrInvalidOAuthState
	}
	var w oauthStateWire
	if err := json.Unmarshal(body, &w); err != nil {
		return OAuthState{}, ErrInvalidOAuthState
	}
	if time.Now().After(time.Unix(w.Exp, 0)) {
		return OAuthState{}, ErrInvalidOAuthState
	}
	if !defaultNonceStore.tryConsume(w.N) {
		return OAuthState{}, ErrInvalidOAuthState
	}
	return OAuthState{Nonce: w.N, Next: w.R}, nil
}

// isSafeNextPath rejects "next" values that could be used as
// open-redirects: external schemes, protocol-relative URLs, anything
// that doesn't look like a single relative path on our site.
func isSafeNextPath(next string) bool {
	if next == "" {
		return true // empty falls through to the post-login default
	}
	if !strings.HasPrefix(next, "/") {
		return false // must be a relative path on our site
	}
	if strings.HasPrefix(next, "//") {
		return false // protocol-relative redirect
	}
	if strings.Contains(next, `\`) {
		return false // several browsers normalize "\" -> "/", so "/\evil" becomes protocol-relative
	}
	if strings.ContainsAny(next, " \t\n\r") {
		return false
	}
	return true
}
