package daemon

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/envelope"
)

// subscribeOrgHeartbeats sets up a NATS subscription on the current
// connection that stamps the local heartbeat cache for every
// <handle>.heartbeat message published in the org — by this daemon or
// any other. This is what makes ppz who cross-daemon-aware: without it
// handleWho only shows agents whose heartbeats arrived via the local
// handleSend path.
//
// Subjects follow the shape <accountID>.<manifold?>.<handle>.heartbeat.
// We subscribe to <accountID>.> and filter client-side for the
// .heartbeat suffix; NATS does not support wildcards in the middle of a
// subject pattern.
//
// Cache-key shape must match handleSend, which stamps with the bare
// handle (req.Handle) — never manifold-prefixed. The handle is the
// second-to-last subject segment by construction (natsubj.BuildSubject
// emits <acct>.<manifold?>.<source>.<pipe>, and heartbeats are always
// source-published). NATS echoes a daemon's own publishes back to its
// own subscriptions, so if we stamped a manifold-prefixed key here the
// publishing daemon would end up with the same agent in its cache
// twice under two different keys, and remote daemons would render
// namespaced agents under unexpected keys.
//
// Each NATS message body is an envelope.Message; the raw heartbeat JSON
// is in the Payload field. We unmarshal the envelope and stamp the cache
// with the inner payload, matching the shape handleSend writes.
//
// Called after every NATS (re)connect — from handleLogin once the
// initial connection is established, and from ensureNATS whenever it
// rebuilds the connection. The prior subscription is destroyed
// automatically when swapNC closes the old nats.Conn.
func (d *Daemon) subscribeOrgHeartbeats(accountID uuid.UUID) {
	prefix := accountID.String() + "."
	_, err := d.NC.Subscribe(prefix+">", func(msg *nats.Msg) {
		d.stampOrgHeartbeat(accountID, msg)
	})
	if err != nil {
		d.recordNATSEvent(NATSEvent{
			Type:   "warn",
			At:     time.Now(),
			Caller: "subscribeOrgHeartbeats",
			NCID:   ncID(d.NC),
			Reason: err.Error(),
		})
	}
}

// stampOrgHeartbeat is the subscription callback body: filter for
// .heartbeat subjects, extract the handle, unwrap the envelope, stamp
// the cache. A method (rather than a closure) so tests can invoke it
// directly to model a callback executing at an arbitrary point relative
// to login/logout — nats.go's Close does not join in-flight callbacks.
func (d *Daemon) stampOrgHeartbeat(accountID uuid.UUID, msg *nats.Msg) {
	subj := msg.Subject
	if !strings.HasSuffix(subj, ".heartbeat") {
		return
	}
	parts := strings.Split(subj, ".")
	// Need at least <acct>.<handle>.heartbeat for there to be a
	// handle segment at all.
	if len(parts) < 3 {
		return
	}
	handle := parts[len(parts)-2]
	if handle == "" {
		return
	}
	var env envelope.Message
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return
	}
	d.Heartbeats.Stamp(handle, env.Payload, time.Now())
}
