package daemon

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/envelope"
)

func payloads(msgs []cliproto.ReadMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Payload
	}
	return out
}

// sortRetainedByPriority reorders the delivered window high-first
// (1 < 2 < 3 on EffectivePriority). Stable: FIFO stream-sequence order
// is preserved within a tier, and unset (0) messages interleave with
// explicit mediums exactly as they arrived.
func TestSortRetainedByPriority_HighFirst(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{Payload: "low", Priority: 3},
		{Payload: "medium", Priority: 2},
		{Payload: "high", Priority: 1},
	}
	sortRetainedByPriority(retained)
	want := []string{"high", "medium", "low"}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSortRetainedByPriority_UnsetInterleavesWithMediumFIFO(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{Payload: "unset-a", Priority: 0},
		{Payload: "medium-b", Priority: 2},
		{Payload: "high", Priority: 1},
		{Payload: "unset-c", Priority: 0},
	}
	sortRetainedByPriority(retained)
	want := []string{"high", "unset-a", "medium-b", "unset-c"}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// All-equal input must come out byte-identical — the design invariant
// that a mesh where nobody sets priority behaves exactly like today.
func TestSortRetainedByPriority_AllEqualIsNoOp(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{Payload: "first"},
		{Payload: "second"},
		{Payload: "third"},
	}
	sortRetainedByPriority(retained)
	want := []string{"first", "second", "third"}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("all-equal input reordered: %v, want %v", got, want)
	}
}

// Same-tier messages keep arrival order even when other tiers sort
// around them (stability across a real mix).
func TestSortRetainedByPriority_StableWithinTier(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{Payload: "high-a", Priority: 1},
		{Payload: "low-a", Priority: 3},
		{Payload: "high-b", Priority: 1},
		{Payload: "low-b", Priority: 3},
	}
	sortRetainedByPriority(retained)
	want := []string{"high-a", "high-b", "low-a", "low-b"}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Garbage priorities (written straight onto NATS by a foreign publisher,
// bypassing handleSend) clamp to medium — no super-priority tier.
func TestSortRetainedByPriority_GarbageClampsToMedium(t *testing.T) {
	retained := []cliproto.ReadMessage{
		{Payload: "garbage-neg", Priority: -5},
		{Payload: "high", Priority: 1},
		{Payload: "garbage-big", Priority: 99},
	}
	sortRetainedByPriority(retained)
	want := []string{"high", "garbage-neg", "garbage-big"}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// shouldSortByPriority gates the sort to message-shaped reads only:
//   - never in follow mode (--tail must keep ONE ordering discipline for
//     the whole stream — the live half can't be reordered, so the drained
//     backlog isn't either);
//   - never for byte-faithful pipes (stdout / stdin / stdctrl / custom):
//     WIRE.md §8 promises those replay in arrival order, byte-for-byte;
//   - uncollared reads (BareTarget set) mirror the CLI's tabular default
//     (read.go render switch), so they do sort.
func TestShouldSortByPriority(t *testing.T) {
	cases := []struct {
		name       string
		follow     bool
		channel    string
		bareTarget string
		want       bool
	}{
		{"inbox drain", false, "inbox", "", true},
		{"broadcast drain", false, "broadcast", "", true},
		{"uncollared drain", false, "", "room", true},
		{"inbox follow", true, "inbox", "", false},
		{"uncollared follow", true, "", "room", false},
		{"stdout pipe", false, "stdout", "", false},
		{"stdin pipe", false, "stdin", "", false},
		{"stdctrl pipe", false, "stdctrl", "", false},
		{"custom pipe", false, "mylog", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldSortByPriority(c.follow, c.channel, c.bareTarget); got != c.want {
				t.Fatalf("shouldSortByPriority(%v, %q, %q) = %v, want %v",
					c.follow, c.channel, c.bareTarget, got, c.want)
			}
		})
	}
}

// TestSortRetainedByPriority_StableAboveInsertionThreshold guards FIFO-
// within-tier at a slice size where Go's sort switches OFF insertion sort
// (which is incidentally stable) onto pdqsort's quicksort path (which is
// NOT). With ≤12 elements sort.Slice and sort.SliceStable are byte-
// identical, so the smaller stability tests above cannot actually catch a
// SliceStable→Slice regression. This one can: 21 messages, three tiers
// interleaved, each tier's arrival order (sequence number) must survive.
func TestSortRetainedByPriority_StableAboveInsertionThreshold(t *testing.T) {
	tiers := []int{1, 2, 3} // high, medium, low cycled
	var retained []cliproto.ReadMessage
	for i := 0; i < 21; i++ {
		retained = append(retained, cliproto.ReadMessage{
			Payload:  fmt.Sprintf("%02d", i),
			Priority: tiers[i%3],
		})
	}
	sortRetainedByPriority(retained)

	// Expected: all highs (i%3==0) in arrival order, then mediums (i%3==1),
	// then lows (i%3==2) — each block ascending by sequence number.
	var want []string
	for _, tier := range []int{0, 1, 2} {
		for i := 0; i < 21; i++ {
			if i%3 == tier {
				want = append(want, fmt.Sprintf("%02d", i))
			}
		}
	}
	if got := payloads(retained); !reflect.DeepEqual(got, want) {
		t.Fatalf("21-element sort broke within-tier FIFO:\n got %v\nwant %v", got, want)
	}
}

// TestReadMessageFromEnvelope pins the single envelope→ReadMessage
// projection shared by the drain and follow paths — including Priority,
// whose follow-path copy is otherwise only exercised by live --tail reads.
func TestReadMessageFromEnvelope(t *testing.T) {
	env := envelope.New("alpha", "status", "hello", time.Date(2026, 5, 7, 12, 34, 56, 0, time.UTC))
	env.InReplyTo = "reply-id"
	env.AckRequested = true
	env.Priority = 1

	rm := readMessageFromEnvelope(env)
	if rm.ID != env.ID || rm.Sender != "alpha" || rm.Subject != "status" || rm.Payload != "hello" {
		t.Fatalf("core fields not projected: %+v", rm)
	}
	if rm.InReplyTo != "reply-id" || !rm.AckRequested {
		t.Fatalf("reply/ack fields not projected: %+v", rm)
	}
	if rm.Priority != 1 {
		t.Fatalf("Priority not projected: got %d, want 1 (both read paths depend on this)", rm.Priority)
	}
	if rm.CreatedAt != "2026-05-07T12:34:56Z" {
		t.Fatalf("CreatedAt = %q, want stable second-precision UTC", rm.CreatedAt)
	}
}
