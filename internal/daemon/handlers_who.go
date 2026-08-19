package daemon

import (
	"context"
	"encoding/json"
	"net"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// handleWho returns the daemon's in-memory heartbeat snapshot — the
// data backing `ppz who`. Like handleDiag, this verb must work even
// when the daemon has no NATS connection: an operator looking up "who
// was running before my daemon got sick" should always get an answer.
//
// Owner enrichment: we fetch /api/v1/sources and join each cached
// heartbeat's handle to its source's CreatedBy. Doing it at query
// time (rather than embedding owner in the heartbeat payload) means
// transfer-of-ownership server-side reflects in `ppz who` on the next
// call without restarting agents. If the server call fails — daemon
// offline, server down, NATS broken — we fall through with empty
// owners rather than fail the whole verb; the cache snapshot is still
// the more useful answer.
//
// All filter / colour / format work happens client-side in cmdWho.
func (d *Daemon) handleWho(ctx context.Context, conn net.Conn, params json.RawMessage) {
	var req cliproto.WhoRequest
	_ = json.Unmarshal(params, &req) // currently empty; reserved for future scoping

	// Scoped to the current account: every stamp is tagged with the
	// account it arrived under, and rows from any other account —
	// pre-login leftovers, straggler callbacks from a closed
	// subscription — must never render. Logged out ⇒ no current
	// account ⇒ no rows.
	cache := d.Heartbeats.SnapshotAccount(d.State.AccountID())

	var lr cliproto.ListSourcesReply
	_ = d.callServer(ctx, "GET", "/api/v1/sources", nil, &lr)

	writeIPC(conn, cliproto.WhoReply{Entries: enrichEntriesWithOwners(cache, lr.Sources)})
}
