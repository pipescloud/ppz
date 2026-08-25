package natsauth

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/pipescloud/ppz/internal/acl"
)

// ACL Phase 3 — the enforcement claims, proven against a real NATS
// server rather than asserted about permission strings.
//
// The compiler tests in internal/acl pin what goes INTO the credential.
// These pin what the server does with it, which is the only thing that
// actually stops a hand-rolled client.

// connectWithPerms mints a user JWT in the tenant's account carrying
// exactly the given permissions, and connects with it.
func connectWithPerms(t *testing.T, url string, tn tenant, p acl.Permissions) *nats.Conn {
	t.Helper()
	exp := time.Now().Add(5 * time.Minute).Unix()
	jwtStr, seedStr, err := MintUserJWTWithPermissions(tn.accPub, tn.signKP, "acl-user", p, exp)
	if err != nil {
		t.Fatalf("mint scoped user: %v", err)
	}
	kp, err := nkeys.FromSeed([]byte(seedStr))
	if err != nil {
		t.Fatalf("seed parse: %v", err)
	}
	nc, err := nats.Connect(url,
		nats.UserJWT(
			func() (string, error) { return jwtStr, nil },
			func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) },
		),
		nats.Timeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("connect with scoped creds: %v", err)
	}
	return nc
}

func isPermissionErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "permission")
}

// Write-without-read, proven. This is the claim the whole lattice rests
// on: grant pub on the subject, withhold the JetStream API, and the
// principal can send and provably cannot read back.
func TestNATSEnforcement_WriteOnlyPrincipal(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	subject := acme.accPub + ".alice.inbox"
	perms := acl.Permissions{
		PubAllow: []string{subject},
		SubAllow: []string{"_INBOX.>"},
		PubDeny:  []string{"$JS.API.>"},
	}
	nc := connectWithPerms(t, url, acme, perms)
	defer nc.Close()

	if err := nc.Publish(subject, []byte("hello")); err != nil {
		t.Fatalf("write-only principal must be able to publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after publish: %v", err)
	}

	// The read half: creating a consumer is how reads actually happen.
	_, err := nc.Request("$JS.API.CONSUMER.CREATE.pipe_x_alice_inbox", []byte("{}"), time.Second)
	if err == nil {
		t.Fatal("write-only principal created a consumer — read/write are not separated")
	}
}

// The mirror: an observer of a shared terminal must not be able to type
// into it.
func TestNATSEnforcement_ReadOnlyPrincipalCannotPublish(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	subject := acme.accPub + ".alice.stdout"
	perms := acl.Permissions{
		PubAllow: []string{"$JS.API.CONSUMER.CREATE.pipe_x_alice_stdout.>"},
		SubAllow: []string{subject, "_INBOX.>"},
	}
	nc := connectWithPerms(t, url, acme, perms)
	defer nc.Close()

	errCh := make(chan error, 1)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errCh <- err })

	_ = nc.Publish(subject, []byte("keystroke"))
	_ = nc.Flush()

	select {
	case err := <-errCh:
		if !isPermissionErr(err) {
			t.Errorf("expected a permissions violation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("read-only principal published to a terminal's stdout with no violation")
	}
}

// A principal with no grant on a pipe must not be able to inspect its
// stream — the first thing a read does.
func TestNATSEnforcement_UngrantedPrincipalCannotStreamInfo(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	perms := acl.Permissions{
		PubAllow: []string{"$JS.API.STREAM.INFO.pipe_x_mine"},
		SubAllow: []string{"_INBOX.>"},
	}
	nc := connectWithPerms(t, url, acme, perms)
	defer nc.Close()

	if _, err := nc.Request("$JS.API.STREAM.INFO.pipe_x_theirs", nil, time.Second); err == nil {
		t.Fatal("principal inspected a stream it holds no grant on")
	}
}

// Stream enumeration carries no stream token, so it cannot be scoped per
// pipe and is denied outright. Without this, any member could list every
// pipe name and message count in the account regardless of grants.
func TestNATSEnforcement_CannotEnumerateStreams(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	perms := acl.Permissions{
		PubAllow: []string{"$JS.API.STREAM.INFO.pipe_x_mine"},
		SubAllow: []string{"_INBOX.>"},
		PubDeny:  []string{"$JS.API.STREAM.LIST", "$JS.API.STREAM.NAMES"},
	}
	nc := connectWithPerms(t, url, acme, perms)
	defer nc.Close()

	for _, subj := range []string{"$JS.API.STREAM.LIST", "$JS.API.STREAM.NAMES"} {
		if _, err := nc.Request(subj, nil, time.Second); err == nil {
			t.Errorf("%s succeeded — stream enumeration must be denied", subj)
		}
	}
}

// Stream lifecycle stays server-side. This is also the JS-API
// control-plane hole flagged in docs/AUTH-V2.md §Phase 3.5: a user JWT
// with pub $JS.API.> could PURGE any stream in the account.
func TestNATSEnforcement_CannotPurgeOrDeleteStreams(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	perms := acl.Permissions{
		PubAllow: []string{"$JS.API.STREAM.INFO.pipe_x_mine"},
		SubAllow: []string{"_INBOX.>"},
		PubDeny: []string{
			"$JS.API.STREAM.PURGE.>", "$JS.API.STREAM.DELETE.>",
			"$JS.API.STREAM.CREATE.>", "$JS.API.STREAM.UPDATE.>",
		},
	}
	nc := connectWithPerms(t, url, acme, perms)
	defer nc.Close()

	for _, subj := range []string{
		"$JS.API.STREAM.PURGE.pipe_x_mine",
		"$JS.API.STREAM.DELETE.pipe_x_mine",
	} {
		if _, err := nc.Request(subj, nil, time.Second); err == nil {
			t.Errorf("%s succeeded — stream lifecycle must stay server-side", subj)
		}
	}
}

// The unenforced credential must still work exactly as today. Same
// upgrade-safety property as the server-side gate test, proven against a
// real server: if this fails, every existing org breaks on deploy.
func TestNATSEnforcement_UnrestrictedCredentialStillWorks(t *testing.T) {
	url, stop, acme, _ := startTestNATSWithTwoAccounts(t)
	defer stop()

	nc := connectWithPerms(t, url, acme, acl.Permissions{
		PubAllow: []string{">"}, SubAllow: []string{">"},
	})
	defer nc.Close()

	subject := acme.accPub + ".anything.at.all"
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("unrestricted credential could not subscribe: %v", err)
	}
	if err := nc.Publish(subject, []byte("x")); err != nil {
		t.Fatalf("unrestricted credential could not publish: %v", err)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("unrestricted credential did not deliver: %v", err)
	}
}
