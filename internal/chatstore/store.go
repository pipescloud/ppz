// Package chatstore is the on-disk store behind `ppz chat`: per-identity chat
// history, the list of added pipes, and read markers — one JSON file per
// conversation window under $PPZ_HOME/chat/<handle>/. JetStream stays the
// source of truth for received messages; this store is authoritative only for
// the bits it can't hand back — your outbound sends and your read position.
//
// Windows are loaded into memory on Open, mutated there (with in-memory
// dedup), and written back on Flush (atomic temp+rename) — so the launch
// replay burst is O(1) per message, not a file rewrite each time.
package chatstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Window kinds. A window is uniquely identified by (Kind, Name): an agent DM
// keyed by the counterparty handle, or an uncollared pipe keyed by its target.
const (
	KindAgent  = "agent"
	KindSource = "source" // message-kind source: a bare inbox (human/service)
	KindPipe   = "pipe"
)

// Message directions.
const (
	DirIn  = "in"  // received
	DirOut = "out" // sent by me (never replayable from the wire)
)

// Message is one stored envelope. CreatedAt is the daemon's RFC3339-UTC stamp,
// which sorts lexically.
type Message struct {
	ID        string `json:"id"`
	Dir       string `json:"dir"`
	Sender    string `json:"sender"`
	Subject   string `json:"subject,omitempty"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// Window is a roster entry as the store sees it.
type Window struct {
	Kind   string
	Name   string
	Label  string
	Unread int
}

// windowFile is the on-disk JSON shape of one window.
type windowFile struct {
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Label      string    `json:"label"`
	LastReadAt string    `json:"last_read_at"`
	Messages   []Message `json:"messages"`
}

type windowState struct {
	kind, name, label string
	lastReadAt        string
	messages          []Message
	ids               map[string]bool
	dirty             bool
}

// unread counts received messages newer than the read marker. Outbound never
// counts. An empty read marker sorts before every real timestamp, so all
// inbound is unread until the window is first read.
func (w *windowState) unread() int {
	n := 0
	for i := range w.messages {
		if w.messages[i].Dir == DirIn && w.messages[i].CreatedAt > w.lastReadAt {
			n++
		}
	}
	return n
}

// Store is a per-identity chat store rooted at <home>/chat/<handle>/.
type Store struct {
	dir  string
	wins map[string]*windowState // key = makeKey(kind, name)
}

func makeKey(kind, name string) string { return kind + "\x00" + name }

// fileName maps (kind, name) to a filesystem-safe file. The kind prefix keeps
// an agent and a pipe of the same name in separate files; the authoritative
// kind/name still live inside the JSON, so a lossy sanitisation only risks a
// (practically impossible, given handle/pipe naming) filename collision.
func fileName(kind, name string) string {
	prefix := "a_"
	if kind == KindPipe {
		prefix = "p_"
	}
	return prefix + sanitize(name) + ".json"
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// Open resolves (and creates) <home>/chat/<handle>/ and loads any existing
// window files into memory.
// Open is account-agnostic (tests / legacy callers): equivalent to
// OpenForAccount with an empty account.
func Open(home, handle string) (*Store, error) {
	return OpenForAccount(home, "", handle)
}

// OpenForAccount resolves (and creates) <home>/chat/<account>/<handle>/ and
// loads any existing window files. Scoping by account isolates chat state
// across orgs/servers — a handle is unique only within its account.
func OpenForAccount(home, account, handle string) (*Store, error) {
	// sanitize(account) keeps it filesystem-safe; an empty account collapses
	// (filepath.Join drops "") to the legacy <home>/chat/<handle> path.
	dir := filepath.Join(home, "chat", sanitize(account), handle)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, wins: map[string]*windowState{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var wf windowFile
		if json.Unmarshal(b, &wf) != nil || wf.Name == "" {
			continue // skip a corrupt/partial file rather than fail the whole store
		}
		ws := &windowState{kind: wf.Kind, name: wf.Name, label: wf.Label, lastReadAt: wf.LastReadAt, ids: map[string]bool{}}
		for _, m := range wf.Messages {
			if ws.ids[m.ID] {
				continue
			}
			ws.ids[m.ID] = true
			ws.messages = append(ws.messages, m)
		}
		s.wins[makeKey(wf.Kind, wf.Name)] = ws
	}
	return s, nil
}

// win returns the (kind, name) window, creating it if needed. A non-empty
// label updates the stored one.
func (s *Store) win(kind, name, label string) *windowState {
	k := makeKey(kind, name)
	ws := s.wins[k]
	if ws == nil {
		ws = &windowState{kind: kind, name: name, label: label, ids: map[string]bool{}, dirty: true}
		s.wins[k] = ws
		return ws
	}
	if label != "" && ws.label != label {
		ws.label = label
		ws.dirty = true
	}
	return ws
}

// Windows lists every known conversation with its current unread count,
// deterministically ordered (agents before pipes, then by name).
func (s *Store) Windows() ([]Window, error) {
	out := make([]Window, 0, len(s.wins))
	for _, ws := range s.wins {
		out = append(out, Window{Kind: ws.kind, Name: ws.name, Label: ws.label, Unread: ws.unread()})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Ingest records a message in the (kind, name) window, creating the window if
// needed. Idempotent by Message.ID. Returns whether it was newly added. The
// write is buffered in memory; call Flush to persist.
func (s *Store) Ingest(kind, name, label string, m Message) (bool, error) {
	ws := s.win(kind, name, label)
	if ws.ids[m.ID] {
		return false, nil
	}
	ws.ids[m.ID] = true
	ws.messages = append(ws.messages, m)
	ws.dirty = true
	return true, nil
}

// Messages returns a window's messages in chronological (CreatedAt) order.
func (s *Store) Messages(kind, name string) ([]Message, error) {
	ws := s.wins[makeKey(kind, name)]
	if ws == nil {
		return nil, nil
	}
	out := make([]Message, len(ws.messages))
	copy(out, ws.messages)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// Unread counts received messages newer than the window's read marker.
func (s *Store) Unread(kind, name string) (int, error) {
	ws := s.wins[makeKey(kind, name)]
	if ws == nil {
		return 0, nil
	}
	return ws.unread(), nil
}

// MarkRead advances the window's read marker to its newest message (unread→0).
func (s *Store) MarkRead(kind, name string) error {
	ws := s.wins[makeKey(kind, name)]
	if ws == nil {
		return nil
	}
	newest := ws.lastReadAt
	for i := range ws.messages {
		if ws.messages[i].CreatedAt > newest {
			newest = ws.messages[i].CreatedAt
		}
	}
	if newest != ws.lastReadAt {
		ws.lastReadAt = newest
		ws.dirty = true
	}
	return nil
}

// AddPipe creates an (initially empty) pipe window so it shows in the roster
// before any message arrives.
func (s *Store) AddPipe(name, label string) error {
	s.win(KindPipe, name, label)
	return nil
}

// RemovePipe forgets a pipe window and deletes its file.
func (s *Store) RemovePipe(name string) error {
	delete(s.wins, makeKey(KindPipe, name))
	err := os.Remove(filepath.Join(s.dir, fileName(KindPipe, name)))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Flush writes every dirty window to disk (atomic temp+rename per file).
func (s *Store) Flush() error {
	for _, ws := range s.wins {
		if !ws.dirty {
			continue
		}
		if err := s.writeWindow(ws); err != nil {
			return err
		}
		ws.dirty = false
	}
	return nil
}

// Migrate re-keys a window from fromKind to toKind (same name): a simple
// re-key when the target is absent, or a merge (dedup by id, keeping the later
// read marker) when it exists. The old file is deleted; the target is written
// on the next Flush. Used when chat learns a handle it first saw as an agent DM
// is actually a message source, so a restart can't rebuild two windows for one
// counterparty.
func (s *Store) Migrate(fromKind, toKind, name string) error {
	fromKey := makeKey(fromKind, name)
	from := s.wins[fromKey]
	if from == nil {
		return nil
	}
	if to := s.wins[makeKey(toKind, name)]; to != nil {
		// Merge from → to: dedup by id, keep the later read marker.
		for _, m := range from.messages {
			if !to.ids[m.ID] {
				to.ids[m.ID] = true
				to.messages = append(to.messages, m)
			}
		}
		if from.lastReadAt > to.lastReadAt {
			to.lastReadAt = from.lastReadAt
		}
		to.dirty = true
	} else {
		// Simple re-key: move the window under the new kind.
		from.kind = toKind
		from.dirty = true
		s.wins[makeKey(toKind, name)] = from
	}
	delete(s.wins, fromKey)
	if err := os.Remove(filepath.Join(s.dir, fileName(fromKind, name))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) writeWindow(ws *windowState) error {
	b, err := json.MarshalIndent(windowFile{
		Kind: ws.kind, Name: ws.name, Label: ws.label,
		LastReadAt: ws.lastReadAt, Messages: ws.messages,
	}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, fileName(ws.kind, ws.name))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
