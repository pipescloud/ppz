package acl

// The enable preview — ACL Phase 3.
//
// Enforcement is opt-in per org. This is what makes flipping it a
// deliberate act rather than a surprise: it reports what access
// disappears the moment `acl_enforced` goes true.
//
// Computed from the derived default table, never from observed traffic.
// Traffic-based inference is wrong in both directions — silent on a
// collaboration that happens to be idle right now, and noisy about a
// one-off read from months ago.

import (
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/pipescloud/ppz/internal/natsubj"
)

// SourceRef is one handle in the account and who owns it.
type SourceRef struct {
	Handle        string
	Kind          string // "pty" | "message"
	OwnerID       uuid.UUID
	OwnerIsMember bool
}

// PreviewInput is everything the preview needs, already read out of the
// database. Kept as plain data so the reporting logic stays pure.
type PreviewInput struct {
	OrgOwnerID            uuid.UUID
	OrgOwnerIsPlaceholder bool
	Members               []Principal
	Sources               []SourceRef
	Pipes                 []PipeRef
}

// OrphanRef is a handle whose owner has left the org.
type OrphanRef struct {
	Handle string
	Owner  string // "" when the owner is no longer resolvable
}

// TerminalRef is a shared terminal that becomes private.
type TerminalRef struct {
	Handle string
	Owner  string
}

// PipeLoss is one pipe losing access for everyone but its owner.
type PipeLoss struct {
	Pipe  string
	Owner string
}

// Preview is the report shown before enabling.
type Preview struct {
	// Reported first and loudest: Evaluate returns nothing for a
	// non-member BEFORE it checks handle ownership, so these pipes
	// become reachable only by org owner/admin and the nominal owner
	// loses their own handles. It is the case that locks people out
	// with no obvious cause.
	OrphanedHandles []OrphanRef

	// InsertAccount falls back to the 'unauthenticated' placeholder
	// when an org is created without an owner. Such an org has no real
	// principal holding implicit admin, so nobody could fix it after.
	PlaceholderOwnedOrg bool

	SharedTerminals   []TerminalRef
	InboxReadLoss     []PipeLoss
	CollaredUserPipes []PipeLoss

	affected []string
}

// IsEmpty reports whether enabling would change nothing.
func (p Preview) IsEmpty() bool {
	return !p.PlaceholderOwnedOrg &&
		len(p.OrphanedHandles) == 0 &&
		len(p.SharedTerminals) == 0 &&
		len(p.InboxReadLoss) == 0 &&
		len(p.CollaredUserPipes) == 0
}

// AllAffectedPipes is every pipe path that loses access for someone.
// Uncollared pipes are shared org space and are never listed — including
// them would bury the signal under rows that change nothing.
func (p Preview) AllAffectedPipes() []string {
	out := append([]string(nil), p.affected...)
	sort.Strings(out)
	return out
}

// stdioPipes lose access entirely for non-owners.
var stdioPipes = map[string]bool{
	"stdin": true, "stdout": true, "stdctrl": true, "system": true,
}

// BuildPreview reports what enabling enforcement would take away.
func BuildPreview(in PreviewInput) Preview {
	var p Preview
	p.PlaceholderOwnedOrg = in.OrgOwnerIsPlaceholder

	names := make(map[uuid.UUID]string, len(in.Members))
	for _, m := range in.Members {
		names[m.ID] = m.Name
	}
	sources := make(map[string]SourceRef, len(in.Sources))
	for _, s := range in.Sources {
		sources[s.Handle] = s
		if !s.OwnerIsMember {
			p.OrphanedHandles = append(p.OrphanedHandles, OrphanRef{
				Handle: s.Handle, Owner: names[s.OwnerID],
			})
		}
		if s.Kind == "pty" {
			p.SharedTerminals = append(p.SharedTerminals, TerminalRef{
				Handle: s.Handle, Owner: names[s.OwnerID],
			})
		}
	}

	// Only a member other than the owner can actually lose something.
	// In a single-member org there is nobody to lose it.
	othersExist := len(in.Members) > 1

	mark := func(path string) { p.affected = append(p.affected, path) }

	for _, pipe := range in.Pipes {
		parts := strings.Split(pipe.Path, ".")
		if len(parts) < 2 {
			continue // uncollared at the root — unaffected
		}
		src, collared := sources[parts[0]]
		if !collared {
			continue // uncollared (or namespaced) shared space — unaffected
		}
		name := parts[len(parts)-1]
		owner := names[src.OwnerID]

		switch {
		case name == natsubj.PresencePipe:
			// Presence stays readable org-wide; `ppz who` is unaffected.
		case name == "inbox":
			// Non-owners keep write and lose read. Worth reporting
			// separately from a total loss: sending still works, so the
			// failure looks different when it happens.
			if othersExist {
				p.InboxReadLoss = append(p.InboxReadLoss, PipeLoss{Pipe: pipe.Path, Owner: owner})
				mark(pipe.Path)
			}
		case stdioPipes[name]:
			if othersExist {
				mark(pipe.Path)
			}
		case !natsubj.AutoProvisionedPipes[name]:
			if othersExist {
				p.CollaredUserPipes = append(p.CollaredUserPipes, PipeLoss{Pipe: pipe.Path, Owner: owner})
				mark(pipe.Path)
			}
		}
	}
	return p
}
