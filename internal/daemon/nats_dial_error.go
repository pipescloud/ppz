package daemon

import (
	"errors"
	"strings"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// controlLineRejection is the server's canonical text for a CONNECT
// whose control line — which carries the whole User JWT — overruns
// max_control_line.
//
// Matched as a string because nats.go exposes no sentinel for it: only
// the auth errors get mapped to ErrXXX values (checkAuthError), and
// everything else is passed through as a plain error wrapping the
// server's `-ERR` text verbatim.
const controlLineRejection = "maximum control line exceeded"

// natsDialError classifies a failed dial into the error the user should
// actually see.
//
// The default remains E_NATS_UNREACHABLE, whose remediation assumes a
// transient or environmental fault. A control-line rejection is neither:
// it is deterministic, it is caused by the size of this principal's
// compiled ACL grants, and no amount of reconnecting or re-logging-in
// changes the credential's size. Flattening it into "unreachable" cost
// a production debugging session, so it gets its own code.
func natsDialError(err error) *cliproto.Error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), controlLineRejection) {
		return cliproto.New(cliproto.ECredentialTooLarge)
	}
	return cliproto.New(cliproto.ENATSUnreachable)
}

// setDialErrLocked records why the last dial failed. Callers must hold
// ncMu — the field is read in the same critical section as d.NC so a
// caller can never see a connection and a stale cause together.
func (d *Daemon) setDialErrLocked(e *cliproto.Error) { d.lastDialErr = e }

// clearDialErrLocked forgets the recorded cause after a dial succeeds.
func (d *Daemon) clearDialErrLocked() { d.lastDialErr = nil }

// clearDialErr is the ncMu-taking form, for callers outside a dial.
func (d *Daemon) clearDialErr() {
	d.ncMu.Lock()
	defer d.ncMu.Unlock()
	d.clearDialErrLocked()
}

// noConnErrLocked is the error to report when no connection is
// installed: the recorded dial cause when there is one, else the
// generic unreachable. Callers must hold ncMu.
func (d *Daemon) noConnErrLocked() *cliproto.Error {
	if d.lastDialErr != nil {
		return d.lastDialErr
	}
	return cliproto.New(cliproto.ENATSUnreachable)
}

// ensureNATSError preserves the classified error ensureNATS produced.
//
// ensureNATS already returns a *cliproto.Error carrying the real cause
// (E_NOT_LOGGED_IN, or the dial classification from rebuildNC). Two
// resolveSendTarget call sites discarded it for a blanket
// E_NATS_UNREACHABLE, which is how `ppz send` kept advising a re-login
// that could not work. handleSubsWait and handleList already did this
// by hand; this is that idiom, named.
func ensureNATSError(err error) *cliproto.Error {
	var ce *cliproto.Error
	if errors.As(err, &ce) {
		return ce
	}
	return cliproto.New(cliproto.ENATSUnreachable)
}
