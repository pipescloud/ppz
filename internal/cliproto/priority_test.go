package cliproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// EffectivePriority is the single place the "0 (unset) and anything
// out-of-range mean medium" rule lives. Sorting keys on it; nothing
// else needs to care. Legacy envelopes decode priority to 0 and MUST
// interleave exactly like explicit mediums.
func TestEffectivePriority(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 2},  // unset / legacy → medium
		{1, 1},  // high
		{2, 2},  // medium
		{3, 3},  // low
		{-5, 2}, // garbage below range → medium
		{4, 2},  // just above range → medium
		{99, 2}, // garbage above range → medium
	}
	for _, c := range cases {
		if got := EffectivePriority(c.in); got != c.want {
			t.Errorf("EffectivePriority(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ReadMessage must always emit `priority` (no omitempty) so the
// `ppz read --json` shape is stable — envelope.Message tests don't
// cover this struct.
func TestReadMessage_MarshalAlwaysIncludesPriority(t *testing.T) {
	b, err := json.Marshal(ReadMessage{ID: "abc", Payload: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"priority":0`) {
		t.Fatalf("unset priority must appear in ReadMessage JSON: %s", b)
	}
	b2, err := json.Marshal(ReadMessage{ID: "abc", Payload: "hi", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"priority":1`) {
		t.Fatalf("populated priority missing from ReadMessage JSON: %s", b2)
	}
}

func TestEInvalidPriority_StableSurface(t *testing.T) {
	if string(EInvalidPriority) != "E_INVALID_PRIORITY" {
		t.Fatalf("EInvalidPriority wire string = %q, want E_INVALID_PRIORITY", EInvalidPriority)
	}
	if got := ExitCode(EInvalidPriority); got == 1 {
		t.Errorf("ExitCode(EInvalidPriority) = 1 (generic); want a dedicated non-1 exit code")
	}
	if msg := Message(EInvalidPriority); msg == "" || msg == "unknown error" {
		t.Errorf("Message(EInvalidPriority) is missing or generic: %q", msg)
	}
	e := New(EInvalidPriority)
	if e == nil || e.Code != EInvalidPriority {
		t.Fatalf("New(EInvalidPriority) returned %+v", e)
	}
}
