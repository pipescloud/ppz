package daemon

import (
	"testing"
	"time"
)

// TestDropsLastHour_ExcludesSwapAttributedDisconnects pins the
// drops_last_hour semantics revision from the 2026-08-19 incident
// report. The daemon swaps its NATS connection on every JWT refresh
// (~every 4.5 min), and each swap closes the old conn — which fires a
// nats.go disconnect for a connection the daemon retired ON PURPOSE.
// natsStatusSnapshot counted those, so a completely healthy hour read
// drops_last_hour=14 and looked like instability (the report's "worth
// separating refresh swaps from genuine drops in that counter").
//
// Contract: a disconnect is a DROP only if the daemon did not initiate
// it. A disconnect whose NCID is named as the retired conn (old=<id>)
// by a swap event just before it is the swap's own teardown — excluded.
// Everything else still counts, including the disconnect caused by a
// reportNATSFailure force_close: that connection was genuinely unusable,
// which is exactly what the counter exists to surface.
//
// RED: every disconnect in the last hour counts — got 3, want 2.
func TestDropsLastHour_ExcludesSwapAttributedDisconnects(t *testing.T) {
	now := time.Now()
	d := &Daemon{NATSEvents: newNATSEventRing(natsEventRingCap)}
	// Append directly to the ring — recordNATSEvent would side-effect
	// (kickReconnect on "closed") and this test is about counting only.
	for _, ev := range []NATSEvent{
		// Routine JWT-refresh rotation: swap retires 0xA, then nats.go
		// reports the close of 0xA that swapNCLocked itself caused.
		// Same shape and reason format as production (see swapNCLocked).
		{Type: "swap", At: now.Add(-10 * time.Minute), Caller: "OnRefreshed-callback", NCID: "0xB", Reason: "old=0xA new=0xB"},
		{Type: "disconnect", At: now.Add(-10*time.Minute + 50*time.Millisecond), Caller: "nats.go", NCID: "0xA", Reason: ""},
		{Type: "closed", At: now.Add(-10*time.Minute + 50*time.Millisecond), Caller: "nats.go", NCID: "0xA", Reason: ""},

		// A genuine network drop: no daemon event precedes it. Counts.
		{Type: "disconnect", At: now.Add(-5 * time.Minute), Caller: "nats.go", NCID: "0xC", Reason: "read tcp: connection reset by peer"},

		// A reportNATSFailure force-close of a zombie conn: daemon-
		// initiated, but the connection was genuinely dead. Counts.
		{Type: "force_close", At: now.Add(-3 * time.Minute), Caller: "reportNATSFailure", NCID: "0xD", Reason: "nats: timeout"},
		{Type: "disconnect", At: now.Add(-3*time.Minute + 10*time.Millisecond), Caller: "nats.go", NCID: "0xD", Reason: ""},
		{Type: "closed", At: now.Add(-3*time.Minute + 10*time.Millisecond), Caller: "nats.go", NCID: "0xD", Reason: ""},
	} {
		d.NATSEvents.Append(ev)
	}

	_, drops, _ := d.natsStatusSnapshot()
	if drops != 2 {
		t.Fatalf("drops_last_hour = %d, want 2: the swap-retired conn 0xA must not count as a drop (refresh rotations are routine), while the genuine drop 0xC and the force-closed zombie 0xD must", drops)
	}
}
