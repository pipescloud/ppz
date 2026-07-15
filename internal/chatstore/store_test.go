package chatstore

import "testing"

func msg(id, dir, sender, payload, created string) Message {
	return Message{ID: id, Dir: dir, Sender: sender, Payload: payload, CreatedAt: created}
}

func TestOpen_EmptyStore(t *testing.T) {
	s, err := Open(t.TempDir(), "james")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Windows()
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 0 {
		t.Fatalf("new store should have no windows, got %d", len(w))
	}
}

// Ingest is idempotent by message id — a replayed/reconnected message must not
// duplicate or re-inflate unread (the core fix for "everything unread again").
func TestIngest_Idempotent(t *testing.T) {
	s, _ := Open(t.TempDir(), "james")
	m := msg("m1", "in", "alice", "hi", "2026-07-09T09:00:00Z")
	first, _ := s.Ingest(KindAgent, "alice", "alice", m)
	dup, _ := s.Ingest(KindAgent, "alice", "alice", m)
	if !first {
		t.Errorf("first ingest should report added=true")
	}
	if dup {
		t.Errorf("duplicate id should report added=false")
	}
	got, _ := s.Messages(KindAgent, "alice")
	if len(got) != 1 {
		t.Fatalf("want 1 message after duplicate ingest, got %d", len(got))
	}
}

// Messages come back in chronological order regardless of ingest order (a live
// outbound send can interleave with a later-arriving replayed inbound).
func TestMessages_SortedByCreatedAt(t *testing.T) {
	s, _ := Open(t.TempDir(), "james")
	s.Ingest(KindAgent, "alice", "alice", msg("b", "out", "you", "second", "2026-07-09T09:01:00Z"))
	s.Ingest(KindAgent, "alice", "alice", msg("a", "in", "alice", "first", "2026-07-09T09:00:00Z"))
	s.Ingest(KindAgent, "alice", "alice", msg("c", "in", "alice", "third", "2026-07-09T09:02:00Z"))
	got, _ := s.Messages(KindAgent, "alice")
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].ID != want {
			t.Errorf("pos %d: want %s, got %s (order %v)", i, want, got[i].ID, ids(got))
		}
	}
}

// Unread counts inbound-after-read-marker only; outbound never counts; MarkRead
// zeroes it; a subsequent inbound bumps it again.
func TestUnread_InboundOnly_AndMarkRead(t *testing.T) {
	s, _ := Open(t.TempDir(), "james")
	s.Ingest(KindAgent, "alice", "alice", msg("a", "in", "alice", "1", "2026-07-09T09:00:00Z"))
	s.Ingest(KindAgent, "alice", "alice", msg("b", "in", "alice", "2", "2026-07-09T09:01:00Z"))
	s.Ingest(KindAgent, "alice", "alice", msg("c", "out", "you", "reply", "2026-07-09T09:02:00Z"))
	if u, _ := s.Unread(KindAgent, "alice"); u != 2 {
		t.Errorf("want 2 unread (inbound only), got %d", u)
	}
	if err := s.MarkRead(KindAgent, "alice"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Unread(KindAgent, "alice"); u != 0 {
		t.Errorf("want 0 unread after MarkRead, got %d", u)
	}
	s.Ingest(KindAgent, "alice", "alice", msg("d", "in", "alice", "new", "2026-07-09T09:03:00Z"))
	if u, _ := s.Unread(KindAgent, "alice"); u != 1 {
		t.Errorf("want 1 unread after new inbound, got %d", u)
	}
}

func TestAddRemovePipe(t *testing.T) {
	s, _ := Open(t.TempDir(), "james")
	if err := s.AddPipe("room-1", "room-1"); err != nil {
		t.Fatal(err)
	}
	if !hasWindow(t, s, KindPipe, "room-1") {
		t.Fatalf("added pipe not listed")
	}
	s.Ingest(KindPipe, "room-1", "room-1", msg("p1", "in", "bob", "yo", "2026-07-09T09:00:00Z"))
	if got, _ := s.Messages(KindPipe, "room-1"); len(got) != 1 {
		t.Fatalf("pipe message not stored")
	}
	if err := s.RemovePipe("room-1"); err != nil {
		t.Fatal(err)
	}
	if hasWindow(t, s, KindPipe, "room-1") {
		t.Fatalf("removed pipe still listed")
	}
}

// The actual bug: history (including my own sent messages) and the read marker
// must survive a restart — a fresh Store on the same dir sees them.
func TestPersistsAcrossReopen(t *testing.T) {
	home := t.TempDir()
	s, _ := Open(home, "james")
	s.Ingest(KindAgent, "alice", "alice", msg("a", "in", "alice", "1", "2026-07-09T09:00:00Z"))
	s.Ingest(KindAgent, "alice", "alice", msg("b", "out", "you", "my reply", "2026-07-09T09:01:00Z"))
	s.MarkRead(KindAgent, "alice")
	s.Ingest(KindAgent, "alice", "alice", msg("c", "in", "alice", "later", "2026-07-09T09:02:00Z"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(home, "james")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s2.Messages(KindAgent, "alice")
	if len(got) != 3 {
		t.Fatalf("want 3 messages after reopen (incl. my sent reply), got %d: %v", len(got), ids(got))
	}
	if u, _ := s2.Unread(KindAgent, "alice"); u != 1 {
		t.Errorf("want 1 unread after reopen (read marker persisted), got %d", u)
	}
}

func ids(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func hasWindow(t *testing.T, s *Store, kind, name string) bool {
	t.Helper()
	ws, err := s.Windows()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Kind == kind && w.Name == name {
			return true
		}
	}
	return false
}

// Migrate re-keys an agent window to a source window when there's no existing
// target: the message moves, the old kind is gone, and it survives a reopen.
func TestMigrate_RekeysWindow(t *testing.T) {
	home := t.TempDir()
	s, _ := Open(home, "james")
	s.Ingest(KindAgent, "laurent", "laurent", msg("a", "in", "laurent", "hi", "2026-07-10T09:00:00Z"))
	if err := s.Migrate(KindAgent, KindSource, "laurent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Messages(KindAgent, "laurent"); len(got) != 0 {
		t.Errorf("agent window should be gone after migrate, got %d msgs", len(got))
	}
	if got, _ := s.Messages(KindSource, "laurent"); len(got) != 1 {
		t.Fatalf("source window should hold the migrated message, got %d", len(got))
	}
	s.Flush()
	s2, _ := Open(home, "james")
	if got, _ := s2.Messages(KindAgent, "laurent"); len(got) != 0 {
		t.Errorf("reopen: stray agent window persisted, %d msgs", len(got))
	}
	if got, _ := s2.Messages(KindSource, "laurent"); len(got) != 1 {
		t.Errorf("reopen: source window missing, %d msgs", len(got))
	}
}

// Migrate merges into an existing target window (dedup by id), removing the old.
func TestMigrate_MergesIntoExisting(t *testing.T) {
	s, _ := Open(t.TempDir(), "james")
	s.Ingest(KindAgent, "laurent", "laurent", msg("a", "in", "laurent", "before", "2026-07-10T09:00:00Z"))
	s.Ingest(KindSource, "laurent", "laurent", msg("b", "in", "laurent", "after", "2026-07-10T09:02:00Z"))
	if err := s.Migrate(KindAgent, KindSource, "laurent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Messages(KindAgent, "laurent"); len(got) != 0 {
		t.Errorf("agent window not removed after merge, %d msgs", len(got))
	}
	got, _ := s.Messages(KindSource, "laurent")
	if len(got) != 2 {
		t.Fatalf("merged source should hold both messages, got %d", len(got))
	}
}

// Chat state is isolated per account: the same handle in a different org (or on
// a different server) must not see the previous account's history.
func TestOpenForAccount_IsolatesByAccount(t *testing.T) {
	home := t.TempDir()
	open := func(account string) *Store {
		t.Helper()
		s, err := OpenForAccount(home, account, "james")
		if err != nil {
			t.Fatalf("open %s: %v", account, err)
		}
		return s
	}
	a := open("orgA")
	a.Ingest(KindAgent, "alice", "alice", msg("m1", "in", "alice", "hi", "2026-07-15T09:00:00Z"))
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, _ := open("orgB").Messages(KindAgent, "alice"); len(got) != 0 {
		t.Fatalf("orgB must not see orgA's chat state, got %d msgs", len(got))
	}
	if got, _ := open("orgA").Messages(KindAgent, "alice"); len(got) != 1 {
		t.Fatalf("orgA reopen should see its own state, got %d", len(got))
	}
}
