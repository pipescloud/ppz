package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// Regression: pipescloud production, 2026-08-31.
//
// rebuildNC discarded the dial error entirely and returned a bare
// E_NATS_UNREACHABLE. When ACL Phase 3 pushed the User JWT past the
// server's max_control_line, every connect failed in the protocol
// parser — but the user was told "expired credentials (try 'ppz daemon
// logout' then re-login)". Re-login mints an identically oversized
// credential, so the advice could never work, and the real cause
// ("maximum control line exceeded") was never shown anywhere the user
// could see it.
//
// Two properties are pinned here: the dial error is CLASSIFIED rather
// than flattened, and the classification survives to the call sites
// that later find no connection installed.

func codeOf(t *testing.T, err error) cliproto.Code {
	t.Helper()
	var ce *cliproto.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a *cliproto.Error, got %T: %v", err, err)
	}
	return ce.Code
}

func TestNATSDialError_ClassifiesOversizedCredential(t *testing.T) {
	// nats.go has no sentinel for this: the server's `-ERR 'maximum
	// control line exceeded'` is passed through as a plain error.
	err := natsDialError(errors.New("nats: maximum control line exceeded"))
	if got := codeOf(t, err); got != cliproto.ECredentialTooLarge {
		t.Fatalf("control-line rejection must classify as %s, got %s", cliproto.ECredentialTooLarge, got)
	}
}

func TestNATSDialError_FallsBackToUnreachable(t *testing.T) {
	for _, e := range []error{
		errors.New("dial tcp 10.0.0.1:4222: connect: connection refused"),
		nats.ErrNoServers,
		nil,
	} {
		if got := codeOf(t, natsDialError(e)); got != cliproto.ENATSUnreachable {
			t.Fatalf("ordinary dial failure %v must stay %s, got %s", e, cliproto.ENATSUnreachable, got)
		}
	}
}

func TestNATSDialError_DoesNotAdviseRelogin(t *testing.T) {
	var ce *cliproto.Error
	errors.As(natsDialError(errors.New("nats: maximum control line exceeded")), &ce)
	if strings.Contains(strings.ToLower(ce.Message), "logout") {
		t.Fatalf("an oversized credential is not fixable by re-login; message must not advise it: %q", ce.Message)
	}
	if !strings.Contains(strings.ToLower(ce.Message), "control line") {
		t.Fatalf("message must name the real cause so it is searchable: %q", ce.Message)
	}
}

func TestRebuildNC_SurfacesOversizedCredential(t *testing.T) {
	d := &Daemon{
		NATSURL: "nats://ppz.test:4222",
		dial: func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
			return nil, errors.New("nats: maximum control line exceeded")
		},
	}
	if got := codeOf(t, d.rebuildNC("test")); got != cliproto.ECredentialTooLarge {
		t.Fatalf("rebuildNC must not flatten the dial cause; got %s", got)
	}
}

// TestJetStream_SurfacesLastDialCause is the user-visible symptom: after
// a failed dial there is no connection installed, and every JetStream
// entry point (this is what `ppz ls` hits) reported the generic
// unreachable error instead of the reason the dial actually failed.
func TestJetStream_SurfacesLastDialCause(t *testing.T) {
	d := &Daemon{
		NATSURL: "nats://ppz.test:4222",
		dial: func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
			return nil, errors.New("nats: maximum control line exceeded")
		},
	}
	_ = d.rebuildNC("test")

	_, cerr := d.jetStream()
	if cerr == nil {
		t.Fatal("jetStream() must fail when no connection is installed")
	}
	if cerr.Code != cliproto.ECredentialTooLarge {
		t.Fatalf("jetStream() must report why the dial failed, not a generic %s; got %s",
			cliproto.ENATSUnreachable, cerr.Code)
	}
}

// A later successful dial must clear the remembered cause, so a
// transient failure cannot pin a stale diagnosis onto healthy calls.
func TestJetStream_ForgetsCauseAfterNoDialFailure(t *testing.T) {
	d := &Daemon{
		NATSURL: "nats://ppz.test:4222",
		dial: func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
			return nil, errors.New("nats: maximum control line exceeded")
		},
	}
	_ = d.rebuildNC("test")
	d.clearDialErr()

	_, cerr := d.jetStream()
	if cerr == nil || cerr.Code != cliproto.ENATSUnreachable {
		t.Fatalf("with no recorded dial failure the generic %s is correct; got %v",
			cliproto.ENATSUnreachable, cerr)
	}
}

// armWatch is the same "no connection installed" shape as jetStream():
// it backs `ppz ls --watch` and `ppz subs wait`, and reported the same
// generic unreachable error.
func TestArmWatch_SurfacesLastDialCause(t *testing.T) {
	d := &Daemon{
		NATSURL: "nats://ppz.test:4222",
		dial: func(string, *RefreshLoop, func(NATSEvent)) (*nats.Conn, error) {
			return nil, errors.New("nats: maximum control line exceeded")
		},
	}
	_ = d.rebuildNC("test")

	_, cerr := d.armWatch("subject.test", nil)
	if cerr == nil {
		t.Fatal("armWatch must fail when no connection is installed")
	}
	if cerr.Code != cliproto.ECredentialTooLarge {
		t.Fatalf("armWatch must report why the dial failed; got %s", cerr.Code)
	}
}

// ensureNATSError guards the other half of the same bug: two
// resolveSendTarget call sites replaced whatever ensureNATS returned
// with a bare E_NATS_UNREACHABLE, so `ppz send` kept giving the
// re-login advice even once `ppz ls` had learned to say why. Two
// sibling sites (handleSubsWait, handleList) already propagated
// correctly — this makes the treatment uniform.
func TestEnsureNATSError_PropagatesClassifiedCause(t *testing.T) {
	in := cliproto.New(cliproto.ECredentialTooLarge)
	if got := ensureNATSError(in); got.Code != cliproto.ECredentialTooLarge {
		t.Fatalf("a classified cause must survive; got %s", got.Code)
	}
}

func TestEnsureNATSError_FallsBackToUnreachable(t *testing.T) {
	if got := ensureNATSError(errors.New("some transport failure")); got.Code != cliproto.ENATSUnreachable {
		t.Fatalf("an unclassified failure stays %s; got %s", cliproto.ENATSUnreachable, got.Code)
	}
}

func TestEnsureNATSError_PreservesNotLoggedIn(t *testing.T) {
	// ensureNATS returns E_NOT_LOGGED_IN when the credential is gone.
	// Flattening that to "nats unreachable" is the same misdiagnosis in
	// a different costume.
	if got := ensureNATSError(cliproto.New(cliproto.ENotLoggedIn)); got.Code != cliproto.ENotLoggedIn {
		t.Fatalf("not-logged-in must not be reported as unreachable; got %s", got.Code)
	}
}
