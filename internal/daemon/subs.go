package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// subscriptions persist a per-session list of subscribed pipe subjects —
// the curated subset that `ppz subs {ls,wait,read}` operate over (an
// agent's inbox-monitor list, a human's "I'm in these rooms" set). Stored
// as a JSON array of subject strings at
//
//	<PPZ_HOME>/subs/<session>.json
//
// Mirrors the cursors subsystem deliberately: file-per-session, in-process
// mutex (only the daemon writes here), and NO in-memory cache — the e2e
// harness wipes the dir between scenarios and a cache would mask the wipe.
//
// Subjects are stored verbatim in the user-facing form the CLI accepts:
// `<handle>.<pipe>` (collared) or a bare uncollared pipe path. No glob
// expansion at storage time — if globs ship later they expand at read-time.
//
// Keying is the load-bearing design point (see docs / the subs design
// brief): auto-subscribe-on-create keys under the HANDLE, while manual
// `subs add` keys under session(req.Session) the same way cursors do.
// This file is keying-agnostic — callers pass whichever session string
// applies.
type subscriptions struct {
	dir string
	mu  sync.Mutex
	// account returns the account id that scopes every read/write —
	// subs live at subs/<account>/<session>.json so a cross-account
	// login can't bind one org's subscription set to another org's
	// subjects (the Cursors precedent: CursorKey embeds accountID).
	// nil, or a func returning "" (logged out), falls back to the
	// legacy un-scoped subs/<session>.json layout; a legacy file is
	// adopted into the account dir on first scoped access, so
	// pre-upgrade single-account users keep their subs.
	account func() string
}

func newSubscriptions(home string) *subscriptions {
	return &subscriptions{dir: filepath.Join(home, "subs")}
}

// sessDir returns the directory session files live in under the
// current account scope (caller holds mutex).
func (s *subscriptions) sessDir() string {
	if s.account != nil {
		if acct := s.account(); acct != "" {
			return filepath.Join(s.dir, acct)
		}
	}
	return s.dir
}

// List returns the session's subscribed subjects, sorted and de-duplicated.
func (s *subscriptions) List(sess string) []string {
	sess = session(sess)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(sess)
}

// Add appends subjects to the session's list, idempotently — a subject
// already present is a no-op. Persists only when something changed (no
// churn on restart-and-re-share). Empty / whitespace-only subjects are
// skipped.
func (s *subscriptions) Add(sess string, subjects ...string) error {
	sess = session(sess)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.loadLocked(sess)
	have := sliceToSet(cur)
	changed := false
	for _, subj := range subjects {
		subj = strings.TrimSpace(subj)
		if subj == "" || have[subj] {
			continue
		}
		have[subj] = true
		cur = append(cur, subj)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked(sess, cur)
}

// Remove drops subjects from the session's list, idempotently — removing an
// absent subject is a no-op. Persists only when something changed.
func (s *subscriptions) Remove(sess string, subjects ...string) error {
	sess = session(sess)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.loadLocked(sess)
	drop := sliceToSet(subjects)
	kept := make([]string, 0, len(cur))
	for _, subj := range cur {
		if !drop[subj] {
			kept = append(kept, subj)
		}
	}
	if len(kept) == len(cur) {
		return nil
	}
	return s.saveLocked(sess, kept)
}

// SweepHandle drops every subject targeting `handle`'s pipes from EVERY
// session's subs file. Called on source destroy (mirrors the cursor sweep)
// so a destroyed handle leaves no zombie subscriptions — in its own session
// file (the auto-sub) or in any user shell that subscribed to its pipes.
//
// A subject targets `handle` when it equals the handle outright or is
// collared under it (`<handle>.<pipe>`).
func (s *subscriptions) SweepHandle(handle string) error {
	if handle == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := handle + "."
	// Sweep the current account's scope, plus the legacy flat layout —
	// a not-yet-adopted legacy file may still reference the handle.
	// Other accounts' dirs are deliberately left alone: handle names
	// are only unique per org, so a same-named source elsewhere must
	// keep its subscriptions.
	dirs := []string{s.sessDir()}
	if dirs[0] != s.dir {
		dirs = append(dirs, s.dir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		for _, e := range entries {
			name := e.Name()
			// `<sess>.json.tmp` write-temps already fail the .json suffix test.
			if e.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			path := filepath.Join(dir, name)
			cur := readSubsFile(path)
			kept := make([]string, 0, len(cur))
			for _, subj := range cur {
				if subj == handle || strings.HasPrefix(subj, prefix) {
					continue
				}
				kept = append(kept, subj)
			}
			if len(kept) != len(cur) {
				if err := writeSubsFile(path, kept); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// loadLocked reads + parses the session file under the current account
// scope, returning subjects sorted. A missing or unparseable file
// yields an empty list (caller holds mutex). When the scoped file is
// missing but a legacy un-scoped one exists, the legacy file is adopted
// (renamed) into the account dir first — pre-account-scoping daemons
// wrote flat subs/<sess>.json, and the overwhelmingly-common
// single-account user should keep their subs across the upgrade.
func (s *subscriptions) loadLocked(sess string) []string {
	dir := s.sessDir()
	path := filepath.Join(dir, sess+".json")
	if dir != s.dir {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			legacy := filepath.Join(s.dir, sess+".json")
			if _, lerr := os.Stat(legacy); lerr == nil {
				if os.MkdirAll(dir, 0o700) == nil {
					_ = os.Rename(legacy, path)
				}
			}
		}
	}
	return readSubsFile(path)
}

// readSubsFile reads + parses one subs file. Missing or unparseable
// yields nil.
func readSubsFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var subs []string
	if json.Unmarshal(data, &subs) != nil {
		return nil
	}
	sort.Strings(subs)
	return subs
}

// saveLocked writes the list atomically (tmp + rename), same as
// cursors, under the current account scope.
func (s *subscriptions) saveLocked(sess string, subs []string) error {
	dir := s.sessDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sort.Strings(subs)
	return writeSubsFile(filepath.Join(dir, sess+".json"), subs)
}

// writeSubsFile writes one subs file atomically (tmp + rename).
func writeSubsFile(path string, subs []string) error {
	data, err := json.Marshal(subs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sliceToSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
