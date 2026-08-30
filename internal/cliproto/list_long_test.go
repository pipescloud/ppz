package cliproto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// `ppz ls -l` (long) surfaces a pipe's retention — the one thing `pipe
// set` could change but nothing could read back. Modelled on `ls -l` in
// Linux: same rows, extra detail columns, opt-in.
//
// The retention columns are TTL / MAXMSGS / MAXBYTES, sited next to
// BUFFERED because they are exactly the caps that bound it.

// CONTRACT: `--json` output must not change unless -l is also given.
// Agents parse `ls --json`; adding keys to its default output is a wire
// change for every existing consumer. The fields are therefore omitempty
// AND left unpopulated by the daemon unless long was requested — the
// omitempty alone would still be a change once anything set them.
func TestPrintListJSON_DefaultCarriesNoRetentionKeys(t *testing.T) {
	sources := []Source{{
		Handle:    "chat",
		CreatedBy: "foo",
		PipeInfos: []PipeInfo{{Pipe: "inbox", Total: 3, Unread: 1}},
	}}
	var buf bytes.Buffer
	PrintListJSONWithUncollared(&buf, sources, nil)

	for _, key := range []string{"ttl_seconds", "max_msgs", "max_bytes"} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("`ls --json` (no -l) emitted %q — default JSON output must be byte-identical to before this feature; got %s", key, buf.String())
		}
	}
}

func TestPrintListJSON_LongCarriesRetentionKeys(t *testing.T) {
	sources := []Source{{
		Handle:    "chat",
		CreatedBy: "foo",
		PipeInfos: []PipeInfo{{Pipe: "inbox", Total: 3, TTLSeconds: 3600, MaxMsgs: 500, MaxBytes: 1048576}},
	}}
	var buf bytes.Buffer
	PrintListJSONWithUncollared(&buf, sources, nil)

	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatalf("unmarshal row: %v (output %s)", err, buf.String())
	}
	for key, want := range map[string]float64{"ttl_seconds": 3600, "max_msgs": 500, "max_bytes": 1048576} {
		got, ok := row[key]
		if !ok {
			t.Errorf("`ls --json -l` row is missing %q: %s", key, buf.String())
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// The short table is the default every existing test and muscle-memory
// depends on: seven columns, no more.
func TestPrintList_ShortTableHeaderUnchanged(t *testing.T) {
	sources := []Source{{Handle: "chat", CreatedBy: "foo", PipeInfos: []PipeInfo{{Pipe: "inbox"}}}}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, sources, nil, false, false)

	header := strings.Fields(strings.SplitN(buf.String(), "\n", 2)[0])
	want := []string{"NAMESPACE", "PIPE", "UNREAD", "BUFFERED", "LAST", "PAYLOAD", "CREATOR"}
	if len(header) != len(want) {
		t.Fatalf("short header = %v, want exactly %v", header, want)
	}
	for i := range want {
		if header[i] != want[i] {
			t.Fatalf("short header = %v, want %v", header, want)
		}
	}
}

// Long adds the three retention columns after BUFFERED, so a cap sits
// beside the count it bounds.
func TestPrintList_LongTableAddsRetentionColumns(t *testing.T) {
	sources := []Source{{
		Handle:    "chat",
		CreatedBy: "foo",
		PipeInfos: []PipeInfo{{Pipe: "inbox", Total: 3, TTLSeconds: 86400, MaxMsgs: 5000, MaxBytes: 16777216}},
	}}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, sources, nil, false, true)

	header := strings.Fields(strings.SplitN(buf.String(), "\n", 2)[0])
	want := []string{"NAMESPACE", "PIPE", "UNREAD", "BUFFERED", "TTL", "MAXMSGS", "MAXBYTES", "LAST", "PAYLOAD", "CREATOR"}
	if len(header) != len(want) {
		t.Fatalf("long header = %v, want %v", header, want)
	}
	for i := range want {
		if header[i] != want[i] {
			t.Fatalf("long header = %v, want %v", header, want)
		}
	}

	row := strings.SplitN(buf.String(), "\n", 3)[1]
	for _, want := range []string{"24h", "5000", "16777216"} {
		if !strings.Contains(row, want) {
			t.Errorf("long row is missing %q: %s", want, row)
		}
	}
}

// Uncollared pipes take the same columns — they are pipes, and `pipe
// set` configures them through their own endpoint.
func TestPrintList_LongCoversUncollaredPipes(t *testing.T) {
	uncollared := []UncollaredPipe{{
		Name: "room",
		Info: PipeInfo{Pipe: "room", Total: 2, TTLSeconds: 3600, MaxMsgs: 50, MaxBytes: 1024},
	}}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, nil, uncollared, false, true)

	if !strings.Contains(buf.String(), "1h") || !strings.Contains(buf.String(), "50") {
		t.Errorf("uncollared long row lost its retention: %s", buf.String())
	}
}

// TTL renders compactly. time.Duration's own String() gives "24h0m0s",
// which is three columns of noise in a table whose whole point is
// scanning caps at a glance.
func TestFormatTTLColumn(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want string
	}{
		{86400, "24h"},
		{3600, "1h"},
		{604800, "168h"},
		{90, "1m30s"},
		{0, "-"},
	} {
		if got := formatTTLColumn(tc.secs); got != tc.want {
			t.Errorf("formatTTLColumn(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}
