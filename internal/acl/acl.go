// Package acl evaluates pipe access: who may read, write or administer
// a pipe, and — just as importantly — why.
//
// Pure by construction. No DB, no NATS, no HTTP. The storage layer
// hands it grants, the transport layer consumes its decisions, and the
// surfaces render its provenance. See docs/ACL.md.
package acl

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Perm is a capability bitset.
//
// read and write are INDEPENDENT — neither implies the other. admin
// implies both, plus the right to manage the pipe's ACL, retention and
// existence.
//
// The independence is load-bearing: `<handle>.inbox` needs write
// without read (anyone may send to alice, only alice reads what
// arrived), and it is enforceable rather than merely declarable because
// NATS keeps the two apart — writes are a bare subject publish, reads
// go entirely through the JetStream API.
type Perm uint8

const (
	Read Perm = 1 << iota
	Write
	Admin
)

func (p Perm) String() string {
	if p == 0 {
		return "none"
	}
	var parts []string
	for _, c := range []struct {
		p Perm
		s string
	}{{Read, "read"}, {Write, "write"}, {Admin, "admin"}} {
		if p&c.p != 0 {
			parts = append(parts, c.s)
		}
	}
	return strings.Join(parts, "+")
}

// EveryoneID is the pseudo-principal an ACL row names to grant the
// whole account. Seeded as a fixed UUID exactly the way the
// 'unauthenticated' placeholder already is, so the FK and the
// uniqueness constraint on acl_grants stay honest.
//
// It lives here rather than in internal/db because this is the pure
// package the evaluator and the storage layer agree through — one
// definition, no drift. It must never equal uuid.Nil, which means
// "unauthenticated" everywhere else: an anonymous caller would then
// match every org-wide grant.
var EveryoneID = uuid.MustParse("00000000-0000-0000-0000-000000000002")

// OrgRole is a principal's standing in the account.
type OrgRole string

const (
	OrgNone   OrgRole = ""
	OrgMember OrgRole = "member"
	OrgAdmin  OrgRole = "admin"
	OrgOwner  OrgRole = "owner"
)

// rank orders roles. Unknown values rank 0 and therefore fail closed.
func (r OrgRole) rank() int {
	switch r {
	case OrgOwner:
		return 3
	case OrgAdmin:
		return 2
	case OrgMember:
		return 1
	default:
		return 0
	}
}

func (r OrgRole) AtLeast(min OrgRole) bool {
	if r.rank() == 0 {
		return false
	}
	return r.rank() >= min.rank()
}

// CanAdministerOrg reports whether the role carries implicit admin on
// every pipe in the account. Computed, never stored, so no revoke can
// lock an owner out of their own org.
func (r OrgRole) CanAdministerOrg() bool { return r == OrgOwner || r == OrgAdmin }

// Principal is the subject of a grant: a human, a service account, or
// @everyone.
type Principal struct {
	ID      uuid.UUID
	Name    string
	OrgRole OrgRole
}

// Subject is the pipe being evaluated.
//
// Path is the subject path — what natsubj.BuildSubject produces, minus
// the account prefix. Collar is the owning handle ("" for uncollared
// pipes) and Owner is that handle's principal.
type Subject struct {
	Path   string
	Collar string
	Name   string
	Owner  uuid.UUID
}

// Grant is one stored ACL row.
type Grant struct {
	PrincipalID uuid.UUID
	Principal   string
	Selector    Selector
	Perm        Perm
	GrantedBy   string
	CreatedAt   time.Time
}

// ReasonKind is where a capability came from.
type ReasonKind string

const (
	ReasonNone        ReasonKind = ""
	ReasonDefault     ReasonKind = "default"
	ReasonGrant       ReasonKind = "grant"
	ReasonHandleOwner ReasonKind = "handle-owner"
	ReasonOrgRole     ReasonKind = "org-role"
	ReasonKeyScope    ReasonKind = "key-scope"
)

// rank decides which explanation survives when several would confer the
// same capability. The more specific one wins, so that revoking a grant
// visibly changes the answer instead of silently falling back to a
// default that reads identically.
func (k ReasonKind) rank() int {
	switch k {
	case ReasonOrgRole:
		return 4
	case ReasonGrant:
		return 3
	case ReasonHandleOwner:
		return 2
	case ReasonDefault:
		return 1
	default:
		return 0
	}
}

// Reason explains one capability — granted or denied.
type Reason struct {
	Kind   ReasonKind
	Grant  *Grant // set when Kind == ReasonGrant
	Detail string // the string every surface prints
}

// Label is the short provenance token used by --json and the VIA
// column.
func (r Reason) Label() string {
	switch r.Kind {
	case ReasonDefault:
		return "default"
	case ReasonGrant:
		return "grant"
	case ReasonHandleOwner:
		return "handle owner"
	case ReasonOrgRole:
		return "org role"
	case ReasonKeyScope:
		return "key scope"
	}
	return ""
}

// Decision is the result of evaluating one principal against one pipe.
//
// Why carries provenance for every capability, granted or denied. It is
// not decoration: with defaults derived from (collar, pipe name) rather
// than stored, most access has NO row behind it, so a surface built on
// the grant table alone renders an almost-empty view and reports that
// nobody can reach alice.inbox when every member can write to it.
type Decision struct {
	Perm Perm
	Why  map[Perm]Reason
}

func (d Decision) Has(p Perm) bool { return d.Perm&p != 0 }

func (d Decision) Reason(p Perm) Reason {
	if d.Why == nil {
		return Reason{}
	}
	return d.Why[p]
}

func (d *Decision) allow(p Perm, r Reason) {
	if d.Why == nil {
		d.Why = map[Perm]Reason{}
	}
	d.Perm |= p
	if existing, ok := d.Why[p]; ok && existing.Kind.rank() >= r.Kind.rank() {
		return
	}
	d.Why[p] = r
}

func (d *Decision) deny(p Perm, detail string) {
	if d.Why == nil {
		d.Why = map[Perm]Reason{}
	}
	if d.Perm&p != 0 {
		return
	}
	d.Why[p] = Reason{Kind: ReasonNone, Detail: detail}
}

// CanSeeRoster reports whether a principal may list who else can reach
// a pipe. Any access at all qualifies — including write-only, which is
// the case most easily got wrong: an inbox sender holds no read, and a
// naive "can you read it" gate would hide the roster from every sender
// in the org. Coordination needs it, and it leaks little, since handle
// and pipe names are already org-visible from `ppz ls`.
func CanSeeRoster(d Decision) bool { return d.Perm != 0 }

// Evaluate resolves a principal's access to a pipe.
func Evaluate(p Principal, s Subject, grants []Grant) Decision {
	var d Decision

	// The account is the tenancy boundary; defaults live inside it.
	if p.OrgRole == OrgNone {
		for _, perm := range []Perm{Read, Write, Admin} {
			d.deny(perm, "not a member of this org")
		}
		return d
	}

	// Org owner and admin hold implicit admin everywhere.
	if p.OrgRole.CanAdministerOrg() {
		r := Reason{Kind: ReasonOrgRole, Detail: "org " + string(p.OrgRole)}
		for _, perm := range []Perm{Read, Write, Admin} {
			d.allow(perm, r)
		}
		return d
	}

	// The collar is the ownership boundary: everything under a handle
	// belongs to that handle's principal.
	if s.Collar != "" && s.Owner != uuid.Nil && s.Owner == p.ID {
		r := Reason{Kind: ReasonHandleOwner, Detail: "handle owner"}
		for _, perm := range []Perm{Read, Write, Admin} {
			d.allow(perm, r)
		}
	} else {
		for perm, detail := range defaultsFor(s) {
			d.allow(perm, Reason{Kind: ReasonDefault, Detail: detail})
		}
	}

	for i := range grants {
		g := grants[i]
		if g.PrincipalID != p.ID && g.PrincipalID != EveryoneID {
			continue
		}
		if !Match(g.Selector, s.Path) {
			continue
		}
		r := Reason{Kind: ReasonGrant, Grant: &g, Detail: "grant by " + g.GrantedBy}
		for _, perm := range expand(g.Perm) {
			d.allow(perm, r)
		}
	}

	for _, perm := range []Perm{Read, Write, Admin} {
		d.deny(perm, denialDetail(s, perm))
	}
	return d
}

// Effective applies the one implication in the lattice — admin carries
// read and write — returning a bitset with those bits actually set.
//
// Evaluate applies this while resolving; Compile needs it too, because
// it is handed raw Perms and an admin-only bitset has neither the read
// nor the write bit set.
func (p Perm) Effective() Perm {
	if p&Admin != 0 {
		return Read | Write | Admin
	}
	return p
}

// expand applies the one implication in the lattice: admin carries read
// and write. Read and write never imply each other.
func expand(p Perm) []Perm {
	if p&Admin != 0 {
		return []Perm{Read, Write, Admin}
	}
	var out []Perm
	if p&Read != 0 {
		out = append(out, Read)
	}
	if p&Write != 0 {
		out = append(out, Write)
	}
	return out
}

// defaultsFor is the derived default table (docs/ACL.md).
//
//	<handle>.inbox                        write: everyone
//	<handle>.heartbeat                    read:  everyone
//	<handle>.stdin|stdout|stdctrl|system  nothing for others
//	<handle>.<user-created>               nothing for others
//	uncollared <manifold>.<name>          read + write: everyone
//
// Nothing here is stored. Deriving rather than seeding an "@everyone
// gets everything" row is what removes deny rules and precedence tiers
// from the design: every stored grant is an allow that widens.
func defaultsFor(s Subject) map[Perm]string {
	if s.Collar == "" {
		return map[Perm]string{
			Read:  "default — org-shared space",
			Write: "default — org-shared space",
		}
	}
	switch s.Name {
	case "inbox":
		return map[Perm]string{Write: "default — inbox is write-open"}
	case "heartbeat":
		return map[Perm]string{Read: "default — presence is read-open"}
	}
	return nil
}

// denialDetail is the string printed after ✗ in `ppz acl whoami`. It is
// the answer to "why can't I read this", which is the question this
// feature generates most often.
func denialDetail(s Subject, p Perm) string {
	if s.Collar == "" {
		return "no grant"
	}
	switch {
	case s.Name == "inbox" && p == Read:
		return "no grant; only the handle owner may read an inbox"
	case s.Name == "heartbeat" && p == Write:
		return "no grant; only the handle owner may publish its heartbeat"
	case p == Admin:
		return "not the handle owner, not an org admin"
	}
	return "no grant; default for <handle>." + s.Name + " is owner-only"
}
