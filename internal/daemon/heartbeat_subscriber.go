package daemon

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/pipescloud/ppz/internal/envelope"
	"github.com/pipescloud/ppz/internal/natsubj"
)

// subscribePresence sets up a NATS subscription on the current
// connection that stamps the local heartbeat cache for every heartbeat
// published in the org — by this daemon or any other. This is what
// makes `ppz who` cross-daemon-aware: without it handleWho only shows
// agents whose heartbeats arrived via the local handleSend path.
//
// ACL Phase 0b — this replaces subscribeOrgHeartbeats, which subscribed
// to <accountID>.> and filtered for a ".heartbeat" suffix client-side.
// Live JetStream publishes are also delivered to core subscribers, so
// that subscription received every message published anywhere in the
// org: every stdout byte of every shared terminal, every inbox message
// between other agents. No JetStream permission could close it, because
// the bytes never touched the JS API — pipe read ACLs were bypassable
// in a line of client code.
//
// Presence now has its own subject family (natsubj.PresencePrefix), so
// this subscription sees heartbeats and nothing else.
//
// Cache-key shape must match handleSend, which stamps with the bare
// handle (req.Handle) — never manifold-prefixed. NATS echoes a daemon's
// own publishes back to its own subscriptions, so a manifold-prefixed
// key here would leave the publishing daemon holding the same agent
// twice under two different keys, and remote daemons rendering
// namespaced agents under unexpected ones. ParsePresenceSubject returns
// the bare handle at any manifold depth.
//
// Each NATS message body is an envelope.Message; the raw heartbeat JSON
// is in the Payload field.
//
// Called after every NATS (re)connect — from handleLogin once the
// initial connection is established, and from ensureNATS whenever it
// rebuilds the connection. The prior subscription is destroyed
// automatically when swapNC closes the old nats.Conn.
//
// Returns the subscription so callers (and tests) can assert its scope;
// the daemon itself does not need to hold it.
func (d *Daemon) subscribePresence(accountID uuid.UUID) (*nats.Subscription, error) {
	sub, err := d.NC.Subscribe(natsubj.PresencePrefix(accountID), func(msg *nats.Msg) {
		d.stampPresence(accountID, msg)
	})
	if err != nil {
		d.recordNATSEvent(NATSEvent{
			Type:   "warn",
			At:     time.Now(),
			Caller: "subscribePresence",
			NCID:   ncID(d.NC),
			Reason: err.Error(),
		})
		return nil, err
	}
	return sub, nil
}

// stampPresence is the subscription callback body: parse the presence
// subject for its bare handle, unwrap the envelope, stamp the cache. A
// method (rather than a closure) so tests can invoke it directly to
// model a callback executing at an arbitrary point relative to
// login/logout — nats.go's Close does not join in-flight callbacks.
//
// The stamp is tagged with the account it arrived under (#193): no lock
// spans the login sequence, so a stale-account stamp can always land
// too late. Tagged, it is inert rather than a ghost in `ppz who`.
func (d *Daemon) stampPresence(accountID uuid.UUID, msg *nats.Msg) {
	handle, ok := natsubj.ParsePresenceSubject(msg.Subject)
	if !ok {
		return
	}
	var env envelope.Message
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return
	}
	d.Heartbeats.Stamp(handle, accountID.String(), env.Payload, time.Now())
}
