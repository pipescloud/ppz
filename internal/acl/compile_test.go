package acl

import (
	"strings"
	"testing"
)

// ACL Phase 3 — compiling effective access into NATS permissions.
//
// This is where an ACL stops being advisory. Everything before it
// describes intent; the compiled credential is what a hand-rolled NATS
// client actually runs into.
//
// The lattice costs nothing to enforce because NATS keeps the two
// halves apart:
//
//   - writes are a bare subject publish, the PubAck landing on the
//     caller's own inbox (internal/daemon/publish.go: js.Publish with
//     no stream lookup anywhere in the path)
//   - reads go entirely through the JetStream API — STREAM.INFO, then
//     CONSUMER.CREATE / CONSUMER.MSG.NEXT (internal/daemon/read.go)
//
// Disjoint permission sets, so write-without-read is enforceable rather
// than merely declarable.

// pipeAt is a fixture helper: one pipe with its subject path and the
// JetStream stream backing it.
func pipeAt(path, stream string) PipeRef {
	return PipeRef{Path: path, Stream: stream}
}

const testAcct = "11111111-1111-1111-1111-111111111111"

func compileOne(path, stream string, p Perm) Permissions {
	return Compile(testAcct, []Access{{Pipe: pipeAt(path, stream), Perm: p}})
}

func hasEntry(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func anyContains(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// A read grant must not confer publish on the pipe's subject.
func TestCompile_ReadGrantsJSAPIOnly(t *testing.T) {
	perms := compileOne("alice.stdout", "pipe_11111111_alice_stdout", Read)

	for _, want := range []string{
		"$JS.API.STREAM.INFO.pipe_11111111_alice_stdout",
		"$JS.API.CONSUMER.CREATE.pipe_11111111_alice_stdout.>",
	} {
		if !hasEntry(perms.PubAllow, want) {
			t.Errorf("read grant missing %q in pub allow: %v", want, perms.PubAllow)
		}
	}
	if hasEntry(perms.PubAllow, testAcct+".alice.stdout") {
		t.Error("a read grant must not confer publish on the pipe's subject")
	}
	if !hasEntry(perms.SubAllow, testAcct+".alice.stdout") {
		t.Error("a read grant must allow the live-tail subscription")
	}
}

// The write-without-read proof at the compiler level: a write grant
// yields the subject publish and nothing that could read the stream
// back.
func TestCompile_WriteGrantsSubjectPubOnly(t *testing.T) {
	perms := compileOne("alice.inbox", "pipe_11111111_alice_inbox", Write)

	if !hasEntry(perms.PubAllow, testAcct+".alice.inbox") {
		t.Fatalf("write grant missing the subject publish: %v", perms.PubAllow)
	}
	for _, forbidden := range []string{"CONSUMER.CREATE", "CONSUMER.MSG.NEXT", "STREAM.MSG.GET", "DIRECT.GET"} {
		if anyContains(perms.PubAllow, forbidden) {
			t.Errorf("write grant leaked read capability %q: %v", forbidden, perms.PubAllow)
		}
	}
	if hasEntry(perms.SubAllow, testAcct+".alice.inbox") {
		t.Error("write grant must not allow subscribing to the pipe — that is a read")
	}
}

// Every principal needs its own inbox for PubAcks and consumer
// delivery. Without it nothing works at all, so it is unconditional.
func TestCompile_AlwaysAllowsOwnInbox(t *testing.T) {
	perms := Compile(testAcct, nil)
	if !hasEntry(perms.SubAllow, "_INBOX.>") {
		t.Errorf("_INBOX.> must always be subscribable: %v", perms.SubAllow)
	}
}

// Presence and the invalidation channel are readable by every member —
// `ppz who` depends on the first, credential refresh on the second.
func TestCompile_AlwaysAllowsPresenceAndSystem(t *testing.T) {
	perms := Compile(testAcct, nil)
	for _, want := range []string{testAcct + "._presence.>", testAcct + "._system.>"} {
		if !hasEntry(perms.SubAllow, want) {
			t.Errorf("missing %q in sub allow: %v", want, perms.SubAllow)
		}
	}
}

// Stream enumeration is account-wide: $JS.API.STREAM.LIST and
// STREAM.NAMES carry no stream token, so per-pipe subject permissions
// cannot restrict them. Denied outright; `ppz ls` goes through the
// ACL-filtered HTTP path instead.
func TestCompile_DeniesStreamEnumeration(t *testing.T) {
	perms := compileOne("alice.stdout", "pipe_11111111_alice_stdout", Admin)
	for _, want := range []string{"$JS.API.STREAM.LIST", "$JS.API.STREAM.NAMES"} {
		if !hasEntry(perms.PubDeny, want) {
			t.Errorf("%q must be denied — it cannot be scoped per pipe: %v", want, perms.PubDeny)
		}
	}
}

// Stream lifecycle stays server-side. The CLI already routes create and
// destroy through HTTP, so denying these in the credential closes the
// JS-API control-plane hole flagged in docs/AUTH-V2.md §Phase 3.5
// without regressing anything.
func TestCompile_DeniesStreamLifecycle(t *testing.T) {
	perms := compileOne("alice.stdout", "pipe_11111111_alice_stdout", Admin)
	for _, want := range []string{
		"$JS.API.STREAM.CREATE.>",
		"$JS.API.STREAM.UPDATE.>",
		"$JS.API.STREAM.DELETE.>",
		"$JS.API.STREAM.PURGE.>",
	} {
		if !hasEntry(perms.PubDeny, want) {
			t.Errorf("%q must be denied even for admin: %v", want, perms.PubDeny)
		}
	}
}

// Admin implies read and write, so it compiles to the union — not to
// some third thing.
func TestCompile_AdminGetsReadAndWriteEntries(t *testing.T) {
	admin := compileOne("alice.notes", "pipe_11111111_alice_notes", Admin)
	if !hasEntry(admin.PubAllow, testAcct+".alice.notes") {
		t.Error("admin missing the write entry")
	}
	if !anyContains(admin.PubAllow, "CONSUMER.CREATE.pipe_11111111_alice_notes") {
		t.Error("admin missing the read entries")
	}
}

// The scaling ceiling, made explicit. Stream names are single tokens
// with dots flattened to underscores, and NATS wildcards match whole
// tokens only — so there is no pattern meaning "every heartbeat
// stream". Broad access must compile to either one allow per stream or
// a broad allow plus one deny per excluded stream, whichever is
// smaller. This test pins that the compiler actually chooses.
func TestCompile_PicksSmallerOfAllowOrDenyList(t *testing.T) {
	// 20 pipes, readable on all but one: a deny-list of 1 beats an
	// allow-list of 20.
	var access []Access
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		p := pipeAt("h."+name, "pipe_11111111_h_"+name)
		perm := Read
		if i == 7 {
			perm = 0
		}
		access = append(access, Access{Pipe: p, Perm: perm})
	}
	perms := Compile(testAcct, access)

	if !anyContains(perms.PubDeny, "pipe_11111111_h_h") {
		t.Errorf("expected the single excluded stream to be denied, not 19 allowed: deny=%v", perms.PubDeny)
	}
	if len(perms.PubAllow) > len(perms.PubDeny)+8 {
		t.Errorf("compiler chose the larger list: %d allow vs %d deny",
			len(perms.PubAllow), len(perms.PubDeny))
	}
}

// ...and the converse: access to one pipe out of many must compile to
// an allow-list, not a deny-list naming every other stream.
func TestCompile_UsesAllowListWhenAccessIsNarrow(t *testing.T) {
	var access []Access
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		perm := Perm(0)
		if i == 3 {
			perm = Read
		}
		access = append(access, Access{Pipe: pipeAt("h."+name, "pipe_11111111_h_"+name), Perm: perm})
	}
	perms := Compile(testAcct, access)

	if anyContains(perms.PubDeny, "pipe_11111111_h_a") {
		t.Errorf("narrow access must not compile to a deny-list of every other stream: deny=%v", perms.PubDeny)
	}
	if !anyContains(perms.PubAllow, "pipe_11111111_h_d") {
		t.Errorf("the one readable stream is missing from the allow list: %v", perms.PubAllow)
	}
}

// A principal with nothing gets a credential that can do nothing except
// its own inbox, presence and the invalidation channel.
func TestCompile_NoAccessYieldsNoPipeEntries(t *testing.T) {
	// Positive control: the same pipe WITH access produces both
	// entries, so a compiler that emits nothing at all cannot pass.
	granted := compileOne("alice.stdout", "pipe_11111111_alice_stdout", Read|Write)
	if !hasEntry(granted.PubAllow, testAcct+".alice.stdout") ||
		!anyContains(granted.PubAllow, "CONSUMER.CREATE.pipe_11111111_alice_stdout") {
		t.Fatalf("control: read+write must produce both entries: %v", granted.PubAllow)
	}

	perms := Compile(testAcct, []Access{{Pipe: pipeAt("alice.stdout", "pipe_11111111_alice_stdout"), Perm: 0}})
	if hasEntry(perms.PubAllow, testAcct+".alice.stdout") {
		t.Error("a principal with no access must not be able to publish")
	}
	if anyContains(perms.PubAllow, "CONSUMER.CREATE.pipe_11111111_alice_stdout") {
		t.Error("a principal with no access must not be able to consume")
	}
}

// Credential size is the scaling limit of the whole approach: it grows
// with the number of pty sources in the account, ~4 entries per shared
// terminal, and it is sent on every reconnect. Benchmarked so the
// ceiling is a number in CI rather than a surprise in production.
func BenchmarkCompile_CredentialSize(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		access := make([]Access, 0, n)
		for i := 0; i < n; i++ {
			s := "pipe_11111111_h_" + strings.Repeat("x", i%8) + string(rune('a'+i%26))
			access = append(access, Access{Pipe: pipeAt("h.p", s), Perm: Read | Write})
		}
		b.Run(strings.Repeat("", 0)+itoa(n), func(b *testing.B) {
			var size int
			for i := 0; i < b.N; i++ {
				p := Compile(testAcct, access)
				size = 0
				for _, l := range [][]string{p.PubAllow, p.PubDeny, p.SubAllow, p.SubDeny} {
					for _, s := range l {
						size += len(s)
					}
				}
			}
			b.ReportMetric(float64(size), "cred-bytes")
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
