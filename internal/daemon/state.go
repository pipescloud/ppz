// Package daemon implements the long-lived ppz daemon: IPC server, on-disk
// state, NATS connection, and HTTP client to ppz-server.
package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// Credentials are persisted at $PPZ_HOME/credentials (mode 0600). AccountID +
// AccountName are stored alongside URL+APIKey so a SIGHUP / file-poller reload
// doesn't drop the resolved org info (the alternative would be re-calling
// /auth/exchange after every reload).
type Credentials struct {
	URL     string `json:"url"`
	APIKey  string `json:"api_key"`
	AccountID   string `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`

	// Auth V2 Phase 3 — short-lived NATS user credentials.
	// Re-fetched periodically by the daemon's refresh goroutine.
	NATSUserJWT  string `json:"nats_user_jwt,omitempty"`
	NATSUserSeed string `json:"nats_user_seed,omitempty"`
}

// Files under $PPZ_HOME the daemon owns. Names are part of the WIRE.md §9
// contract — the test reset script deletes these by name.
const (
	fileCredentials = "credentials"
	fileCurrent     = "current.json"   // session → handle map (was "current")
	fileNamespace   = "namespace.json" // session → namespace (Phase 1.5)
	filePID         = "daemon.pid"
	fileSocket      = "daemon.sock"
)

// State is the daemon's in-memory mirror of on-disk credentials + current
// handle. "current" is keyed by session id (tty / $PPZ_SESSION) so each
// terminal window has its own current source — same scoping as cursors,
// avoids the "new terminal silently inherits a stale current set hours
// ago in another window" footgun.
type State struct {
	mu          sync.RWMutex
	home        string
	creds       *Credentials
	accountID   string // resolved at Login (returned by /auth/exchange)
	accountName string // resolved at Login (human label for status)
	keyPrefix   string // 8 chars; safe to display
	current     map[string]string // Phase 1: session → handle
	// Phase 1.5: session → namespace (manifold). Mirrors `current` but
	// for the namespace slot. Empty value (or missing key) = root.
	currentNamespace map[string]string
	knownPipes       map[string]struct{} // server-side handles cached after List/Create
	// Phase 1.5.1: handle → manifold cache. Populated alongside
	// knownPipes from /api/v1/sources responses. Used by the
	// daemon's send / read paths so they can build the correct
	// manifold-aware subject + stream name for the resolved handle.
	handleManifold   map[string]string
	pipesLoaded      bool
	// Completion cache backing IPCComplete. Populated by
	// refreshSourceCache after every real list/ls; read on the
	// shell-tab hot path. completionLoaded distinguishes "no
	// sources" (empty slice, loaded=true) from "cold daemon"
	// (loaded=false) so the client surfaces stale-empty differently
	// from genuine-empty.
	completionSources []cliproto.CompleteSource
	completionLoaded  bool
	// loginCheck caches the daemon's last server-touching call result.
	// Empty means "no observation yet" (fresh daemon). LoginCheckOK on
	// successful 2xx; LoginCheckInvalid on 401 / E_INVALID_API_KEY.
	// Surfaced by `ppz status` so an already-known auth failure shows
	// up immediately, without status needing its own probe.
	loginCheck string
}

// normSession matches cursors.session — empty session id means "default".
func normSession(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func NewState(home string) *State {
	return &State{
		home:             home,
		current:          map[string]string{},
		currentNamespace: map[string]string{},
		knownPipes:       map[string]struct{}{},
		handleManifold:   map[string]string{},
	}
}

func (s *State) Home() string { return s.home }

func (s *State) Credentials() (*Credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.creds == nil {
		return nil, false
	}
	c := *s.creds
	return &c, true
}

func (s *State) AccountID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accountID
}

func (s *State) AccountName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accountName
}

// LoginCheck returns the cached server-validation result. "" means "not
// observed yet" — callers (e.g. status) should probe.
func (s *State) LoginCheck() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loginCheck
}

// SetLoginCheck records the latest server-validation result. Called from
// callServer based on response status. Idempotent — same value twice is
// fine, but transitions (ok→invalid, invalid→ok) are kept.
func (s *State) SetLoginCheck(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginCheck = state
}

func (s *State) KeyPrefix() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyPrefix
}

func (s *State) Current(session string) string {
	session = normSession(session)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current[session]
}

func (s *State) SetLogin(creds Credentials, accountID, accountName, keyPrefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	creds.AccountID = accountID
	creds.AccountName = accountName
	s.creds = &creds
	s.accountID = accountID
	s.accountName = accountName
	s.keyPrefix = keyPrefix
	s.knownPipes = map[string]struct{}{}
	s.handleManifold = map[string]string{}
	s.pipesLoaded = false
	// The completion vocabulary is per-account too: serving the previous
	// org's source/pipe names as Stale:false under the new org would
	// persist until an ls/send/connect happens to refresh it.
	s.completionSources = nil
	s.completionLoaded = false
	// Login itself is a successful server round-trip — record it as the
	// initial "ok" observation so status shows the right state right away.
	s.loginCheck = cliproto.LoginCheckOK
	return s.persistCredsLocked()
}

func (s *State) SetCurrent(session, handle string) error {
	session = normSession(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current[session] = handle
	return s.persistCurrentLocked()
}

// ClearCurrent drops this session's current. Used by `ppz disconnect`.
// Idempotent — clearing an already-clear session is a no-op.
func (s *State) ClearCurrent(session string) error {
	session = normSession(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.current[session]; !ok {
		return nil
	}
	delete(s.current, session)
	return s.persistCurrentLocked()
}

// CurrentNamespace returns the per-session namespace, empty when unset.
// Phase 1.5 (locked decision #18 / #20).
func (s *State) CurrentNamespace(session string) string {
	session = normSession(session)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentNamespace[session]
}

// SetNamespace stores the per-session namespace. Empty value clears (same
// semantics as ClearNamespace). Persisted to disk.
func (s *State) SetNamespace(session, namespace string) error {
	session = normSession(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if namespace == "" {
		delete(s.currentNamespace, session)
	} else {
		s.currentNamespace[session] = namespace
	}
	return s.persistNamespaceLocked()
}

// ClearNamespace drops this session's namespace. Idempotent.
func (s *State) ClearNamespace(session string) error {
	session = normSession(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.currentNamespace[session]; !ok {
		return nil
	}
	delete(s.currentNamespace, session)
	return s.persistNamespaceLocked()
}

func (s *State) RememberPipe(handle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knownPipes[handle] = struct{}{}
}

// RememberSource is the Phase 1.5.2 superset of RememberPipe — caches both
// the known-handle bit AND the handle→manifold mapping. Callers that have
// just minted a source should prefer this so later same-session sends
// don't trigger a refresh just to learn the source's manifold.
func (s *State) RememberSource(handle, manifold string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knownPipes[handle] = struct{}{}
	s.handleManifold[handle] = manifold
}

// ForgetPipe removes a handle from the known-pipes cache. Called after a
// source is destroyed so the cache doesn't return stale hits.
func (s *State) ForgetPipe(handle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.knownPipes, handle)
}

// ClearCurrentForHandle drops every session whose current equals handle.
// Called after a source destroy so stale per-session pointers don't linger.
func (s *State) ClearCurrentForHandle(handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for sess, h := range s.current {
		if h == handle {
			delete(s.current, sess)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistCurrentLocked()
}

func (s *State) KnowsPipe(handle string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.knownPipes[handle]
	return ok
}

func (s *State) ResetPipes(handles []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knownPipes = make(map[string]struct{}, len(handles))
	for _, h := range handles {
		s.knownPipes[h] = struct{}{}
	}
	s.pipesLoaded = true
}

// ResetSources replaces both the known-handles set AND the per-handle
// manifold cache from a server-supplied source list. Preferred over
// ResetPipes for callers that have full cliproto.Source rows. Phase 1.5.1.
func (s *State) ResetSources(handles []string, manifolds map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knownPipes = make(map[string]struct{}, len(handles))
	for _, h := range handles {
		s.knownPipes[h] = struct{}{}
	}
	s.handleManifold = make(map[string]string, len(manifolds))
	for h, m := range manifolds {
		s.handleManifold[h] = m
	}
	s.pipesLoaded = true
}

// SetCompletionSnapshot replaces the in-memory completion cache. Called
// from refreshSourceCache after every server-touching list — the
// completion verb (handleComplete) reads from it on the hot tab path so
// it never has to hit the network. Sources are stored verbatim; pipe
// lists are expected to already be merged (server-known + auto-pipes).
func (s *State) SetCompletionSnapshot(sources []cliproto.CompleteSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy — the caller may mutate the slice afterwards.
	cp := make([]cliproto.CompleteSource, len(sources))
	for i, src := range sources {
		cp[i] = cliproto.CompleteSource{Handle: src.Handle}
		if len(src.Pipes) > 0 {
			cp[i].Pipes = append([]string(nil), src.Pipes...)
		}
	}
	s.completionSources = cp
	s.completionLoaded = true
}

// CompletionSnapshot returns the cached source/pipe vocabulary used by
// IPCComplete. The boolean is false when the snapshot has never been
// populated (cold daemon) — the handler returns Stale=true so the CLI
// can render the "no suggestions yet" empty state instead of treating
// it as a real "no sources".
func (s *State) CompletionSnapshot() ([]cliproto.CompleteSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.completionLoaded {
		return nil, false
	}
	// Return a defensive copy so callers can't mutate the cache.
	cp := make([]cliproto.CompleteSource, len(s.completionSources))
	for i, src := range s.completionSources {
		cp[i] = cliproto.CompleteSource{Handle: src.Handle}
		if len(src.Pipes) > 0 {
			cp[i].Pipes = append([]string(nil), src.Pipes...)
		}
	}
	return cp, true
}

// HandleManifold returns the cached manifold for a known source handle,
// or "" (root) if the handle isn't cached. Phase 1.5.1.
func (s *State) HandleManifold(handle string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handleManifold[handle]
}

// LoadFromDisk reads credentials and current from $PPZ_HOME. Called at
// startup and on SIGHUP. Missing files mean "not logged in" / "no current".
//
// `current.json` is the session→handle map. The legacy plain-text `current`
// file (pre-per-session) is migrated into session "default" if both exist.
func (s *State) LoadFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stage every read BEFORE mutating anything: reload is
	// all-or-nothing. A transient read failure (EACCES, EMFILE, EIO)
	// or corrupt credentials JSON returns the error with the prior
	// in-memory state intact — otherwise a hiccup is indistinguishable
	// from a logout and the watcher destructively drops NATS and wipes
	// caches for a genuinely-logged-in user.
	var creds *Credentials
	if data, err := os.ReadFile(filepath.Join(s.home, fileCredentials)); err == nil {
		var c Credentials
		if uerr := json.Unmarshal(data, &c); uerr != nil {
			// Corrupt is treated like unreadable, not like logged-out:
			// the only writer is tmp+rename-atomic, so garbage here is
			// a foreign truncation/partial state worth surfacing.
			return uerr
		}
		creds = &c
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	current := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(s.home, fileCurrent)); err == nil {
		_ = json.Unmarshal(data, &current)
		if current == nil {
			current = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Phase 1.5: namespace state. Missing file = no namespaces set
	// (graceful upgrade from v0.30 daemon state).
	currentNamespace := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(s.home, fileNamespace)); err == nil {
		_ = json.Unmarshal(data, &currentNamespace)
		if currentNamespace == nil {
			currentNamespace = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Legacy migration: pre-per-session daemons stored a single plain-text
	// "current" file. If we find it, fold it into the "default" session.
	if data, err := os.ReadFile(filepath.Join(s.home, "current")); err == nil {
		if h := strings.TrimSpace(string(data)); h != "" {
			if _, ok := current["default"]; !ok {
				current["default"] = h
			}
		}
	}

	// Commit.
	s.creds = creds
	s.accountID = ""
	s.accountName = ""
	s.keyPrefix = ""
	if creds != nil {
		s.keyPrefix = keyPrefix(creds.APIKey)
		s.accountID = creds.AccountID
		s.accountName = creds.AccountName
	}
	s.current = current
	s.currentNamespace = currentNamespace
	s.knownPipes = map[string]struct{}{}
	s.handleManifold = map[string]string{}
	s.pipesLoaded = false
	s.completionSources = nil
	s.completionLoaded = false
	// Reload zeros the cache: a daemon that just woke up hasn't talked to
	// the server yet under the new credentials, so status should probe.
	s.loginCheck = ""
	return nil
}

func (s *State) SetAccountID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountID = id
}

func (s *State) persistCurrentLocked() error {
	// Atomic-ish: write tmp then rename. Mirrors cursors.saveLocked().
	data, err := json.Marshal(s.current)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.home, fileCurrent+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.home, fileCurrent))
}

// persistNamespaceLocked mirrors persistCurrentLocked for the namespace map.
// Phase 1.5.
func (s *State) persistNamespaceLocked() error {
	data, err := json.Marshal(s.currentNamespace)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.home, fileNamespace+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.home, fileNamespace))
}

func (s *State) persistCredsLocked() error {
	if s.creds == nil {
		_ = os.Remove(filepath.Join(s.home, fileCredentials))
		return nil
	}
	data, err := json.Marshal(s.creds)
	if err != nil {
		return err
	}
	// Atomic like its persist siblings: a crash mid-write must never
	// leave a truncated credentials file — LoadFromDisk treats corrupt
	// creds as an error, so a partial write would wedge reloads.
	tmp := filepath.Join(s.home, fileCredentials+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.home, fileCredentials))
}

func keyPrefix(plaintext string) string {
	s := strings.TrimPrefix(plaintext, "ppz_")
	if len(s) < 8 {
		return s
	}
	return s[:8]
}
