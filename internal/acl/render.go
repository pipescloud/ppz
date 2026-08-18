package acl

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// The three visibility surfaces.
//
//	by-pipe       `ppz pipe acl ls <pipe>`      who can touch this?
//	by-principal  `ppz acl ls --principal <p>`  what can this agent reach?
//	self          `ppz acl whoami [<pipe>]`     what can I do, and why not?
//
// Renderers live in this pure package rather than in internal/cli or
// internal/cliproto so the GUI can render the same rows without
// duplicating the provenance vocabulary, and so they stay table-testable.

// RosterRow is one principal's standing on a pipe.
type RosterRow struct {
	Principal string
	Decision  Decision
}

// PrincipalRow is one pipe a principal can reach.
type PrincipalRow struct {
	Pipe     string
	Decision Decision
}

// Remediation tells a denied caller how to get unstuck, and who can do
// it. Printing the exact command AND the principals able to run it is
// what makes a denial actionable by an agent: it can ask over that
// principal's inbox instead of failing opaquely.
type Remediation struct {
	Command    string
	RunnableBy []string
}

// WhoamiView is the self-service explanation for one pipe.
type WhoamiView struct {
	Pipe        string
	Principal   string
	Decision    Decision
	Remediation *Remediation
}

// tick renders a capability cell.
func tick(d Decision, p Perm) string {
	if d.Has(p) {
		return "✓"
	}
	return "·"
}

// via is the provenance shown in a table row: the explanation for the
// strongest capability the principal actually holds.
func via(d Decision) string {
	for _, p := range []Perm{Admin, Write, Read} {
		if !d.Has(p) {
			continue
		}
		r := d.Reason(p)
		if r.Kind == ReasonGrant && r.Grant != nil {
			s := "grant by " + r.Grant.GrantedBy
			if !r.Grant.CreatedAt.IsZero() {
				s += " · " + r.Grant.CreatedAt.Format("2006-01-02")
			}
			return s
		}
		if r.Kind == ReasonDefault {
			return r.Detail
		}
		return r.Label()
	}
	return ""
}

// viaLabel is the short provenance token for --json.
func viaLabel(d Decision) string {
	for _, p := range []Perm{Admin, Write, Read} {
		if d.Has(p) {
			return d.Reason(p).Label()
		}
	}
	return ""
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// RenderPipeRoster prints who can reach a pipe. Principals holding
// nothing are omitted — an org of 200 would otherwise render 198 empty
// rows.
func RenderPipeRoster(w io.Writer, pipe string, rows []RosterRow) {
	tw := newTable(w)
	fmt.Fprintln(tw, "PRINCIPAL\tR\tW\tA\tVIA")
	for _, r := range rows {
		if r.Decision.Perm == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Principal,
			tick(r.Decision, Read), tick(r.Decision, Write), tick(r.Decision, Admin),
			via(r.Decision))
	}
	_ = tw.Flush()
}

// RenderPrincipalGrants prints what a principal can reach. Derived
// defaults appear alongside stored grants — showing only grants would
// read as "this agent can reach nothing", the exact misreading these
// surfaces exist to prevent.
func RenderPrincipalGrants(w io.Writer, principal string, rows []PrincipalRow) {
	tw := newTable(w)
	fmt.Fprintln(tw, "PIPE\tR\tW\tA\tVIA")
	for _, r := range rows {
		if r.Decision.Perm == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Pipe,
			tick(r.Decision, Read), tick(r.Decision, Write), tick(r.Decision, Admin),
			via(r.Decision))
	}
	_ = tw.Flush()
}

// RenderWhoami prints one caller's standing on one pipe, with the
// reason for every capability — held or not — and, when something is
// missing, how to get it.
func RenderWhoami(w io.Writer, v WhoamiView) {
	fmt.Fprintf(w, "%s — you are %q\n", v.Pipe, v.Principal)
	for _, c := range []struct {
		p    Perm
		name string
	}{{Read, "read"}, {Write, "write"}, {Admin, "admin"}} {
		mark := "✗"
		if v.Decision.Has(c.p) {
			mark = "✓"
		}
		fmt.Fprintf(w, "  %-6s %s  %s\n", c.name, mark, v.Decision.Reason(c.p).Detail)
	}
	if v.Remediation != nil {
		fmt.Fprintf(w, "\n  to fix:       %s\n", v.Remediation.Command)
		if len(v.Remediation.RunnableBy) > 0 {
			fmt.Fprintf(w, "  runnable by:  %s\n", strings.Join(v.Remediation.RunnableBy, ", "))
		}
	}
}

// ─── JSON ────────────────────────────────────────────────────────────
//
// Agents are the primary consumer of these surfaces; a table scraped by
// regex is a bug waiting to happen. The shapes are pinned by test.

type permsJSON struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
	Admin bool `json:"admin"`
}

func perms(d Decision) permsJSON {
	return permsJSON{Read: d.Has(Read), Write: d.Has(Write), Admin: d.Has(Admin)}
}

func (r RosterRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Principal string `json:"principal"`
		permsJSON
		Via string `json:"via"`
	}{r.Principal, perms(r.Decision), viaLabel(r.Decision)})
}

func (r PrincipalRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Pipe string `json:"pipe"`
		permsJSON
		Via string `json:"via"`
	}{r.Pipe, perms(r.Decision), viaLabel(r.Decision)})
}

func (v WhoamiView) MarshalJSON() ([]byte, error) {
	why := map[string]string{}
	for _, c := range []struct {
		p    Perm
		name string
	}{{Read, "read"}, {Write, "write"}, {Admin, "admin"}} {
		why[c.name] = v.Decision.Reason(c.p).Detail
	}
	type remJSON struct {
		Command    string   `json:"command"`
		RunnableBy []string `json:"runnable_by"`
	}
	var rem *remJSON
	if v.Remediation != nil {
		rem = &remJSON{Command: v.Remediation.Command, RunnableBy: v.Remediation.RunnableBy}
	}
	return json.Marshal(struct {
		Pipe      string `json:"pipe"`
		Principal string `json:"principal"`
		permsJSON
		Why         map[string]string `json:"why"`
		Remediation *remJSON          `json:"remediation,omitempty"`
	}{v.Pipe, v.Principal, perms(v.Decision), why, rem})
}
