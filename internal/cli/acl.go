package cli

// ACL verbs — ACL Phase 2.
//
//	ppz pipe acl ls     <pipe>                 who can touch this pipe?
//	ppz pipe acl grant  <pipe> <principal> <perm>
//	ppz pipe acl revoke <pipe> <principal> [perm|all]
//	ppz acl whoami      [<pipe>]               what can I do here, and why not?
//	ppz acl ls          --principal <name>     what can this principal reach?
//
// Every view reports EFFECTIVE access with provenance. A view built on
// stored grants alone would render almost nothing and imply that nobody
// can reach `<handle>.inbox`, when in fact every member can write to it
// — most access is derived from the pipe's collar and name, with no row
// behind it.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// aclRow is the wire shape shared by the roster and by-principal views.
// Deliberately a flat struct rather than internal/acl's Decision: the
// evaluation happens server-side, and the CLI only renders what came
// back.
type aclRow struct {
	Principal string `json:"principal,omitempty"`
	Pipe      string `json:"pipe,omitempty"`
	Read      bool   `json:"read"`
	Write     bool   `json:"write"`
	Admin     bool   `json:"admin"`
	Via       string `json:"via"`
}

type aclWhoami struct {
	Pipe        string            `json:"pipe"`
	Principal   string            `json:"principal"`
	Read        bool              `json:"read"`
	Write       bool              `json:"write"`
	Admin       bool              `json:"admin"`
	Why         map[string]string `json:"why"`
	Remediation *struct {
		Command    string   `json:"command"`
		RunnableBy []string `json:"runnable_by"`
	} `json:"remediation,omitempty"`
}

func aclCall(req cliproto.ACLRequest) (json.RawMessage, error) {
	var reply cliproto.ACLReply
	if err := daemon.Call(ipcSocket(), cliproto.IPCACL, req, &reply); err != nil {
		return nil, err
	}
	return reply.Body, nil
}

// splitJSONFlag pulls --json out of an arbitrary argument list so the
// positional parsing stays simple.
func splitJSONFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-json" {
			jsonOut = true
			continue
		}
		out = append(out, a)
	}
	return out, jsonOut
}

func tickOf(b bool) string {
	if b {
		return "✓"
	}
	return "·"
}

func printACLRows(body json.RawMessage, first string) error {
	var rows []aclRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tR\tW\tA\tVIA\n", first)
	for _, r := range rows {
		name := r.Principal
		if name == "" {
			name = r.Pipe
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			name, tickOf(r.Read), tickOf(r.Write), tickOf(r.Admin), r.Via)
	}
	return tw.Flush()
}

// cmdPipeACL dispatches `ppz pipe acl <subverb>`.
func cmdPipeACL(args []string) error {
	if len(args) == 0 {
		usageExit("pipe acl")
	}
	rest, jsonOut := splitJSONFlag(args[1:])
	switch args[0] {
	case "ls":
		if len(rest) != 1 {
			usageExit("pipe acl ls")
		}
		body, err := aclCall(cliproto.ACLRequest{Action: cliproto.ACLActionRoster, Pipe: rest[0]})
		if err != nil {
			return err
		}
		if jsonOut {
			fmt.Println(strings.TrimSpace(string(body)))
			return nil
		}
		return printACLRows(body, "PRINCIPAL")

	case "grant", "revoke":
		// revoke's perm is optional — omitting it removes everything
		// that principal holds on the pipe.
		if len(rest) < 2 || (args[0] == "grant" && len(rest) < 3) {
			usageExit("pipe acl " + args[0])
		}
		perm := "all"
		if len(rest) >= 3 {
			perm = rest[2]
		}
		action := cliproto.ACLActionGrant
		if args[0] == "revoke" {
			action = cliproto.ACLActionRevoke
		}
		if _, err := aclCall(cliproto.ACLRequest{
			Action: action, Pipe: rest[0], Principal: rest[1], Perm: perm,
		}); err != nil {
			return err
		}
		if action == cliproto.ACLActionGrant {
			fmt.Printf("granted %s on %s to %s\n", perm, rest[0], rest[1])
		} else {
			fmt.Printf("revoked %s on %s from %s\n", perm, rest[0], rest[1])
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "ppz pipe acl: unknown subcommand %q\n", args[0])
	os.Exit(2)
	return nil
}

// cmdACLGroup dispatches `ppz acl <subverb>`.
func cmdACLGroup(args []string) error {
	if groupHelp("acl", args) {
		return nil
	}
	if len(args) == 0 {
		printHelp(os.Stderr, "acl")
		os.Exit(2)
	}
	rest, jsonOut := splitJSONFlag(args[1:])
	switch args[0] {
	case "whoami":
		if len(rest) != 1 {
			usageExit("acl whoami")
		}
		body, err := aclCall(cliproto.ACLRequest{Action: cliproto.ACLActionWhoami, Pipe: rest[0]})
		if err != nil {
			return err
		}
		if jsonOut {
			fmt.Println(strings.TrimSpace(string(body)))
			return nil
		}
		return printWhoami(body)

	case "ls":
		principal := ""
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--principal" && i+1 < len(rest) {
				principal = rest[i+1]
			}
		}
		if principal == "" {
			usageExit("acl ls")
		}
		body, err := aclCall(cliproto.ACLRequest{Action: cliproto.ACLActionPrincipal, Principal: principal})
		if err != nil {
			return err
		}
		if jsonOut {
			fmt.Println(strings.TrimSpace(string(body)))
			return nil
		}
		return printACLRows(body, "PIPE")
	}
	fmt.Fprintf(os.Stderr, "ppz acl: unknown subcommand %q\n", args[0])
	os.Exit(2)
	return nil
}

// printWhoami renders the self-service explanation. The remediation
// block is what makes a denial actionable by an agent: it names the
// exact command AND who can run it, so the agent can ask over that
// principal's inbox instead of failing opaquely.
func printWhoami(body json.RawMessage) error {
	var v aclWhoami
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	fmt.Printf("%s — you are %q\n", v.Pipe, v.Principal)
	for _, c := range []struct {
		name string
		held bool
	}{{"read", v.Read}, {"write", v.Write}, {"admin", v.Admin}} {
		mark := "✗"
		if c.held {
			mark = "✓"
		}
		fmt.Printf("  %-6s %s  %s\n", c.name, mark, v.Why[c.name])
	}
	if v.Remediation != nil {
		fmt.Printf("\n  to fix:       %s\n", v.Remediation.Command)
		if len(v.Remediation.RunnableBy) > 0 {
			fmt.Printf("  runnable by:  %s\n", strings.Join(v.Remediation.RunnableBy, ", "))
		}
	}
	return nil
}
