package acl

import (
	"testing"

	"github.com/google/uuid"
)

// ACL Phase 3 — the enable preview.
//
// Enforcement is opt-in per org. The preview is what makes flipping it
// a deliberate act rather than a surprise: it reports what access
// disappears the moment `acl_enforced` goes true.
//
// Computed from the derived default table, never from observed traffic.
// Traffic-based inference is wrong in both directions — silent on a
// collaboration that happens to be idle right now, and noisy about a
// one-off read from months ago.

func member(name string) Principal {
	return Principal{ID: uuid.New(), Name: name, OrgRole: OrgMember}
}

// previewOrg builds a small org: foo owns it, bar is a member, and
// alice-the-handle is a pty source owned by foo.
func previewOrg() PreviewInput {
	foo := Principal{ID: uuid.New(), Name: "foo", OrgRole: OrgOwner}
	bar := member("bar")
	return PreviewInput{
		OrgOwnerID: foo.ID,
		Members:    []Principal{foo, bar},
		Sources: []SourceRef{
			{Handle: "alice", Kind: "pty", OwnerID: foo.ID, OwnerIsMember: true},
		},
		Pipes: []PipeRef{
			{Path: "alice.stdout", Stream: "pipe_x_alice_stdout"},
			{Path: "alice.inbox", Stream: "pipe_x_alice_inbox"},
			{Path: "alice.heartbeat", Stream: "presence_x_alice"},
			{Path: "room", Stream: "pipe_x_room"},
		},
	}
}

// The case that locks people out with no obvious cause, so it is
// reported first and loudest: Evaluate returns nothing for a non-member
// BEFORE it checks handle ownership, so a source whose creator has left
// the org becomes reachable only by org owner/admin — and the nominal
// owner loses their own handles.
func TestPreview_ListsOrphanedHandles(t *testing.T) {
	in := previewOrg()
	gone := uuid.New()
	in.Sources = append(in.Sources, SourceRef{
		Handle: "departed", Kind: "message", OwnerID: gone, OwnerIsMember: false,
	})
	in.Pipes = append(in.Pipes, PipeRef{Path: "departed.inbox", Stream: "pipe_x_departed_inbox"})

	p := BuildPreview(in)

	if len(p.OrphanedHandles) == 0 {
		t.Fatal("a source whose creator left the org must be reported as orphaned")
	}
	found := false
	for _, h := range p.OrphanedHandles {
		if h.Handle == "departed" {
			found = true
		}
		if h.Handle == "alice" {
			t.Error("alice's owner is still a member — not orphaned")
		}
	}
	if !found {
		t.Errorf("departed handle missing from %v", p.OrphanedHandles)
	}
}

// InsertAccount falls back to the 'unauthenticated' placeholder when an
// org is created without an owner. Such an org has no real principal
// holding implicit admin, so enabling would leave nobody able to fix it.
func TestPreview_ListsPlaceholderOwnedOrg(t *testing.T) {
	in := previewOrg()
	in.OrgOwnerIsPlaceholder = true

	if !BuildPreview(in).PlaceholderOwnedOrg {
		t.Error("an org owned by the unauthenticated placeholder must be flagged")
	}
	in.OrgOwnerIsPlaceholder = false
	if BuildPreview(in).PlaceholderOwnedOrg {
		t.Error("a normally-owned org must not be flagged")
	}
}

// The largest visible change, and the one an org is most likely to be
// relying on right now: every pty source's stdio becomes owner-only.
func TestPreview_ListsSharedTerminals(t *testing.T) {
	p := BuildPreview(previewOrg())

	if len(p.SharedTerminals) == 0 {
		t.Fatal("a pty source's stdio becomes owner-only and must be reported")
	}
	if p.SharedTerminals[0].Handle != "alice" {
		t.Errorf("expected alice, got %v", p.SharedTerminals)
	}
	if p.SharedTerminals[0].Owner != "foo" {
		t.Errorf("the report must name who retains access, got %q", p.SharedTerminals[0].Owner)
	}
}

// A message source has no stdio, so it must not appear as a shared
// terminal — otherwise the section that matters most is padded with
// rows that change nothing.
func TestPreview_MessageSourceIsNotASharedTerminal(t *testing.T) {
	// Positive control: the pty fixture IS reported, so "reports
	// nothing" cannot satisfy this.
	if len(BuildPreview(previewOrg()).SharedTerminals) == 0 {
		t.Fatal("control: a pty source must be reported as a shared terminal")
	}
	in := previewOrg()
	in.Sources = []SourceRef{{Handle: "bot", Kind: "message", OwnerID: in.OrgOwnerID, OwnerIsMember: true}}
	if len(BuildPreview(in).SharedTerminals) != 0 {
		t.Error("a message source has no stdio and must not be listed as a shared terminal")
	}
}

// Non-owners keep write on an inbox but lose read — worth calling out
// separately from a total loss, because sending still works and the
// failure looks different.
func TestPreview_ListsInboxReadLoss(t *testing.T) {
	p := BuildPreview(previewOrg())
	if len(p.InboxReadLoss) == 0 {
		t.Fatal("non-owners lose read on inboxes; that must be reported")
	}
	if p.InboxReadLoss[0].Pipe != "alice.inbox" {
		t.Errorf("expected alice.inbox, got %v", p.InboxReadLoss)
	}
}

// Uncollared pipes are shared org space and are unaffected. Listing
// them would bury the signal under rows that change nothing.
func TestPreview_OmitsUncollaredPipes(t *testing.T) {
	p := BuildPreview(previewOrg())

	// Positive control: collared pipes ARE listed, so an empty report
	// cannot satisfy the omission below.
	affected := p.AllAffectedPipes()
	if len(affected) == 0 {
		t.Fatalf("control: collared pipes must appear in the affected set")
	}
	for _, s := range affected {
		if s == "room" {
			t.Error("uncollared pipes are unaffected and must not be listed")
		}
	}
}

// An org with nothing collared has nothing to lose, and the preview must
// say so plainly rather than showing an empty scary screen.
func TestPreview_EmptyWhenNothingChanges(t *testing.T) {
	foo := Principal{ID: uuid.New(), Name: "foo", OrgRole: OrgOwner}
	in := PreviewInput{
		OrgOwnerID: foo.ID,
		Members:    []Principal{foo},
		Pipes:      []PipeRef{{Path: "room", Stream: "pipe_x_room"}},
	}
	// Positive control: the standard org is NOT empty, so IsEmpty
	// cannot be a constant true.
	if BuildPreview(previewOrg()).IsEmpty() {
		t.Fatal("control: an org with a pty source and an inbox does lose access")
	}

	p := BuildPreview(in)
	if !p.IsEmpty() {
		t.Errorf("an org with only uncollared pipes loses nothing: %+v", p)
	}
}
