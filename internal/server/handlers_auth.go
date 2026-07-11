package server

// HTTP handlers for the Auth V2 GUI flow:
//
//   GET  /login                       login page (Continue with GitHub)
//   GET  /auth/github/start           mint state, 302 to GitHub authorize URL
//   GET  /auth/github/callback        verify state, exchange code, upsert user, set cookie, 303 to next
//   POST /auth/logout                 clear cookie, 303 to /
//   GET  /me                          JSON of the authed user
//   POST /dev/login?user=<seed-user>  test-only — mint a session for an existing internal user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/db"
)

const sessionTTL = 7 * 24 * time.Hour // 7 days

// ─── /login ─────────────────────────────────────────────────────────

func (s *Server) handleGUILogin(w http.ResponseWriter, r *http.Request) {
	// If already signed in, send them where they were going (or /dashboard).
	if uid, ok := s.verifyRequestSession(r); ok && uid != uuid.Nil {
		next := safeNext(r.URL.Query().Get("next"))
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "login.html", map[string]any{
		"Next": r.URL.Query().Get("next"),
	})
}

// ─── /auth/github/start ────────────────────────────────────────────

func (s *Server) handleAuthGitHubStart(w http.ResponseWriter, r *http.Request) {
	if s.GitHubClientID == "" {
		http.Error(w, "github oauth not configured (PPZ_GITHUB_CLIENT_ID)", 500)
		return
	}
	state, err := MintOAuthState(s.SessionKey, safeNext(r.URL.Query().Get("next")))
	if err != nil {
		http.Error(w, "mint state: "+err.Error(), 500)
		return
	}
	q := url.Values{}
	q.Set("client_id", s.GitHubClientID)
	q.Set("redirect_uri", s.BaseURL+"/auth/github/callback")
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	http.Redirect(w, r, s.GitHubAuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// ─── /auth/github/callback ─────────────────────────────────────────

type ghTokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

type ghUserResp struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func (s *Server) handleAuthGitHubCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	stateTok := r.URL.Query().Get("state")
	if code == "" || stateTok == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	st, err := VerifyOAuthState(s.SessionKey, stateTok)
	if err != nil {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// 1. Exchange code → access token.
	form := url.Values{}
	form.Set("client_id", s.GitHubClientID)
	form.Set("client_secret", s.GitHubClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", s.BaseURL+"/auth/github/callback")

	tokenReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.GitHubTokenURL, strings.NewReader(form.Encode()))
	tokenReq.Header.Set("Accept", "application/json")
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), 502)
		return
	}
	defer tokenResp.Body.Close()
	var tr ghTokenResp
	if err := json.NewDecoder(tokenResp.Body).Decode(&tr); err != nil || tr.AccessToken == "" {
		http.Error(w, "no access_token from github", 502)
		return
	}

	// 2. Fetch the GitHub user profile.
	userReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, s.GitHubUserURL, nil)
	userReq.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	userReq.Header.Set("Accept", "application/json")
	userResp, err := http.DefaultClient.Do(userReq)
	if err != nil {
		http.Error(w, "user fetch: "+err.Error(), 502)
		return
	}
	defer userResp.Body.Close()
	var ghUser ghUserResp
	if err := json.NewDecoder(userResp.Body).Decode(&ghUser); err != nil || ghUser.ID == 0 {
		http.Error(w, "github user payload malformed", 502)
		return
	}

	// 3. Upsert user.
	user, isNew, err := db.UpsertUserByGitHubID(r.Context(), s.Pool,
		ghUser.ID, ghUser.Login, ghUser.Email, ghUser.AvatarURL)
	if err != nil {
		http.Error(w, "upsert user: "+err.Error(), 500)
		return
	}

	// 4. First-time signup → auto-create an org named after the GH login,
	//    with this user as the owner. Idempotent: if an org with that
	//    name already exists, skip.
	if isNew {
		_, err := db.GetAccountByName(r.Context(), s.Pool, user.Username)
		if err != nil {
			if _, err := db.InsertAccount(r.Context(), s.Pool, user.Username, user.ID); err != nil {
				http.Error(w, "create org: "+err.Error(), 500)
				return
			}
		}
	}

	// 5. Set session cookie.
	cookieValue, err := SignSessionCookie(s.SessionKey, SessionPayload{
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(sessionTTL),
	})
	if err != nil {
		http.Error(w, "sign cookie: "+err.Error(), 500)
		return
	}
	s.setSessionCookie(w, cookieValue)

	http.Redirect(w, r, safeNext(st.Next), http.StatusSeeOther)
}

// ─── /auth/logout ─────────────────────────────────────────────────

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ─── /me ──────────────────────────────────────────────────────────

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	uid := UserIDFromCtx(r.Context())
	u, err := db.GetUser(r.Context(), s.Pool, uid)
	if err != nil {
		http.Error(w, "get user", 500)
		return
	}
	out := map[string]any{
		"id":         u.ID.String(),
		"username":   u.Username,
		"email":      u.Email,
		"mode":       string(u.Mode),
		"avatar_url": u.AvatarURL,
	}
	if u.GitHubID != nil {
		out["github_id"] = *u.GitHubID
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── /dev/login (test-only) ───────────────────────────────────────

func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.DevLogin {
		http.NotFound(w, r)
		return
	}
	username := r.URL.Query().Get("user")
	if username == "" {
		http.Error(w, "missing ?user=<seed-username>", 400)
		return
	}
	u, err := db.GetUserByUsername(r.Context(), s.Pool, username)
	if err != nil {
		http.Error(w, fmt.Sprintf("user %q not found", username), 404)
		return
	}
	cookieValue, err := SignSessionCookie(s.SessionKey, SessionPayload{
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(sessionTTL),
	})
	if err != nil {
		http.Error(w, "sign cookie: "+err.Error(), 500)
		return
	}
	s.setSessionCookie(w, cookieValue)
	// A ?next=/path lets a plain GET land you straight on a page (one
	// clickable URL for local manual testing). Reuse the OAuth open-redirect
	// guard (isSafeNextPath) so both paths reject the same unsafe values.
	// Absent/unsafe next, keep the "ok" body the e2e POST path asserts on.
	if next := r.URL.Query().Get("next"); next != "" && isSafeNextPath(next) {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ─── helpers ──────────────────────────────────────────────────────

func (s *Server) setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// safeNext defaults the post-login redirect target. If the supplied
// path passes isSafeNextPath, use it; otherwise send to /dashboard.
func safeNext(next string) string {
	if next == "" || !isSafeNextPath(next) {
		return "/dashboard"
	}
	return next
}
