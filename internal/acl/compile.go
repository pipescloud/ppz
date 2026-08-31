package acl

// Compiling effective access into NATS permissions — ACL Phase 3.
//
// This is where an ACL stops being advisory. Everything before it
// describes intent; the compiled credential is what a hand-rolled NATS
// client actually runs into.
//
// The lattice costs nothing to enforce because NATS keeps the halves
// apart. Writes are a bare subject publish with the PubAck landing on
// the caller's own inbox (internal/daemon/publish.go — js.Publish, no
// stream lookup anywhere in the path). Reads go entirely through the
// JetStream API (internal/daemon/read.go — STREAM.INFO, then
// CONSUMER.CREATE / CONSUMER.MSG.NEXT). Disjoint sets, so
// write-without-read is enforceable rather than merely declarable.

import "strings"

// PipeRef is one pipe: how it is NAMED, where it actually rides, and the
// JetStream stream behind it. The three are not interchangeable.
//
// Path is the logical path an ACL selector matches — `alice.heartbeat`.
// Subject is the NATS subject the pipe is really published to and
// subscribed on, which for presence is `<account>._presence.<handle>`
// rather than the logical path (Phase 0b). Stream is the JetStream
// stream name, which flattens dots to underscores.
//
// Keeping the wire subject here rather than deriving it from Path is
// what stops the compiler emitting permissions for a subject nothing
// uses: an earlier version built pub/sub entries from Path, which
// silently denied every heartbeat publish in an org the moment
// enforcement was switched on.
type PipeRef struct {
	Path    string
	Subject string
	Stream  string
}

// wireSubject is where this pipe is actually published and subscribed.
// Falls back to the account-qualified logical path for the ordinary case
// where the two coincide, and for callers that only care about matching.
func (r PipeRef) wireSubject(accountID string) string {
	if r.Subject != "" {
		return r.Subject
	}
	return accountID + "." + r.Path
}

// Access is a principal's effective permission on one pipe.
type Access struct {
	Pipe PipeRef
	Perm Perm
}

// Permissions is a NATS user's permission set, ready to be signed into
// a user JWT.
type Permissions struct {
	PubAllow []string
	PubDeny  []string
	SubAllow []string
	SubDeny  []string
}

// jsReadPatterns are the JetStream API subjects a read needs. `%s` is
// the stream name — a single token, because stream names flatten dots
// to underscores.
var jsReadPatterns = []string{
	"$JS.API.STREAM.INFO.%s",
	"$JS.API.STREAM.MSG.GET.%s",
	"$JS.API.DIRECT.GET.%s",
	"$JS.API.CONSUMER.CREATE.%s.>",
	"$JS.API.CONSUMER.MSG.NEXT.%s.>",
}

// alwaysDenied never appears in any credential.
//
// STREAM.LIST and STREAM.NAMES carry no stream token, so per-pipe
// subject permissions cannot restrict them at all — allowing them would
// let any member enumerate every pipe name and message count in the
// account regardless of grants. `ppz ls` goes through the ACL-filtered
// HTTP path instead.
//
// Stream lifecycle stays server-side: the CLI already routes create and
// destroy through HTTP, so denying these regresses nothing and closes
// the JS-API control-plane hole flagged in docs/AUTH-V2.md §Phase 3.5,
// where a user JWT holding `pub $JS.API.>` could PURGE any stream in the
// account.
// alwaysAllowed is the floor every credential needs to function at all.
//
// $JS.API.INFO is how a client initialises its JetStream context
// (jetstream.New asks for account info before anything else). Without
// it an enforced principal cannot create a JS context, so it cannot use
// even the access it DOES hold — a write-only inbox sender would fail
// to publish, and the daemon thrashes on reconnects instead of
// receiving a clean per-stream denial.
//
// It exposes account-level JetStream metadata (limits, stream count),
// not any pipe's contents, so it is not per-pipe scopable and not worth
// withholding.
var alwaysAllowed = []string{
	"$JS.API.INFO",
}

var alwaysDenied = []string{
	"$JS.API.STREAM.LIST",
	"$JS.API.STREAM.NAMES",
	"$JS.API.STREAM.CREATE.>",
	"$JS.API.STREAM.UPDATE.>",
	"$JS.API.STREAM.DELETE.>",
	"$JS.API.STREAM.PURGE.>",
}

func expandRead(pattern, stream string) string {
	return strings.Replace(pattern, "%s", stream, 1)
}

// broadRead expands a pattern to cover every stream. `>` must be the
// terminal token in a NATS subject, so a pattern with a trailing `.>`
// collapses rather than producing the malformed `CREATE.>.>`.
func broadRead(pattern string) string {
	return strings.Replace(strings.TrimSuffix(pattern, ".>"), "%s", ">", 1)
}

// Compile turns a principal's effective access across the account into
// a NATS permission set.
//
// `access` is expected to enumerate every pipe in the account, including
// those the principal cannot touch: the excluded set is what makes the
// deny-list representation possible.
func Compile(accountID string, access []Access) Permissions {
	p := Permissions{
		// Every principal needs its own inbox — PubAcks and consumer
		// deliveries land there. Presence keeps `ppz who` working for
		// members with no grants at all, and the system channel is how
		// a credential learns it has been invalidated.
		SubAllow: []string{
			"_INBOX.>",
			accountID + "._presence.>",
			accountID + "._system.>", // invalidation; see natsubj.SystemPrefix
		},
		PubAllow: append([]string(nil), alwaysAllowed...),
		PubDeny:  append([]string(nil), alwaysDenied...),
	}

	var readable, excluded []PipeRef
	for _, a := range access {
		perm := a.Perm.Effective()
		if perm&Write != 0 {
			p.PubAllow = append(p.PubAllow, a.Pipe.wireSubject(accountID))
		}
		if perm&Read != 0 {
			// The live tail is a core subscription on the subject.
			p.SubAllow = append(p.SubAllow, a.Pipe.wireSubject(accountID))
			readable = append(readable, a.Pipe)
			continue
		}
		excluded = append(excluded, a.Pipe)
	}

	// Stream names are single tokens and NATS wildcards match whole
	// tokens, so there is no pattern meaning "every heartbeat stream".
	// Broad read access therefore has two possible shapes, and the
	// cheaper one depends on the ratio:
	//
	//   allow-list — one entry per readable stream
	//   deny-list  — the broad allows, plus one deny per stream NOT readable
	//
	// Costs are counted in units of jsReadPatterns: the allow-list is
	// one unit per readable stream, the deny-list one unit for the broad
	// allows plus one per excluded stream.
	//
	// Ties prefer the allow-list. It is the precise representation, and
	// it fails closed if `access` ever turns out to be incomplete —
	// whereas a broad allow would hand out streams the compiler was
	// never told about.
	if 1+len(excluded) < len(readable) {
		for _, pat := range jsReadPatterns {
			p.PubAllow = append(p.PubAllow, broadRead(pat))
		}
		for _, pipe := range excluded {
			for _, pat := range jsReadPatterns {
				p.PubDeny = append(p.PubDeny, expandRead(pat, pipe.Stream))
			}
		}
		return p
	}
	for _, pipe := range readable {
		for _, pat := range jsReadPatterns {
			p.PubAllow = append(p.PubAllow, expandRead(pat, pipe.Stream))
		}
	}
	return p
}

// Unrestricted is the credential an org gets while enforcement is off —
// today's behaviour, unchanged. Kept here beside Compile so the two
// shapes are read together.
func Unrestricted() Permissions {
	return Permissions{PubAllow: []string{">"}, SubAllow: []string{">"}}
}
