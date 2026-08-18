package cliproto

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// `ppz pipe set` mutates the retention of an EXISTING pipe. Until now
// retention could only be chosen at create time (`pipe create --ttl
// --max-msgs --max-bytes`) and was immutable thereafter.
//
// The request mirrors PipeCreateRequest's shape — same four-role
// addressing (manifold + source handle + name), same three pointer
// overrides — so the CLI, daemon and server share one vocabulary. A nil
// override on the request means "leave this field as it is", NOT "reset
// to default"; the server merges onto the stored row.

func TestPipeSetRequest_MirrorsCreateShape(t *testing.T) {
	rt := reflect.TypeOf(PipeSetRequest{})
	for _, tc := range []struct {
		field string
		want  reflect.Type
		json  string
	}{
		{"Handle", reflect.TypeOf(""), "handle"},
		{"Manifold", reflect.TypeOf(""), "manifold,omitempty"},
		{"SourceHandle", reflect.TypeOf((*string)(nil)), "source_handle,omitempty"},
		{"Name", reflect.TypeOf(""), "name"},
		{"TTLSeconds", reflect.TypeOf((*int)(nil)), "ttl_seconds,omitempty"},
		{"MaxMsgs", reflect.TypeOf((*int)(nil)), "max_msgs,omitempty"},
		{"MaxBytes", reflect.TypeOf((*int64)(nil)), "max_bytes,omitempty"},
		{"Session", reflect.TypeOf(""), "session,omitempty"},
	} {
		f, ok := rt.FieldByName(tc.field)
		if !ok {
			t.Errorf("PipeSetRequest.%s missing", tc.field)
			continue
		}
		if f.Type != tc.want {
			t.Errorf("PipeSetRequest.%s type = %v, want %v", tc.field, f.Type, tc.want)
		}
		if got := f.Tag.Get("json"); got != tc.json {
			t.Errorf("PipeSetRequest.%s json tag = %q, want %q", tc.field, got, tc.json)
		}
	}
}

// The reply carries the RESOLVED retention (all three values, defaults
// filled in) rather than only the fields that changed, so the printed
// line is a complete statement of what the pipe now retains.
func TestPipeSetReply_CarriesResolvedRetention(t *testing.T) {
	rt := reflect.TypeOf(PipeSetReply{})
	for field, want := range map[string]reflect.Type{
		"Handle":     reflect.TypeOf(""),
		"Manifold":   reflect.TypeOf(""),
		"Name":       reflect.TypeOf(""),
		"StreamName": reflect.TypeOf(""),
		"TTLSeconds": reflect.TypeOf(0),
		"MaxMsgs":    reflect.TypeOf(0),
		"MaxBytes":   reflect.TypeOf(int64(0)),
	} {
		f, ok := rt.FieldByName(field)
		if !ok {
			t.Errorf("PipeSetReply.%s missing", field)
			continue
		}
		if f.Type != want {
			t.Errorf("PipeSetReply.%s type = %v, want %v", field, f.Type, want)
		}
	}
}

// PrintPipeSet mirrors PrintPipeCreate's pinned line, swapping the verb:
//
//	updated pipe=<PATH> retention=ttl=<dur>,msgs=<n>,bytes=<b>
//
// Same FormatPipePath rendering so collared/uncollared/manifolded pipes
// read identically across create and set.
func TestPrintPipeSet_PinnedLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply PipeSetReply
		want  string
	}{
		{
			name:  "collared root",
			reply: PipeSetReply{Handle: "chat", Name: "archive", TTLSeconds: 604800, MaxMsgs: 5000, MaxBytes: 16777216},
			want:  "updated pipe=chat.archive retention=ttl=168h0m0s,msgs=5000,bytes=16777216\n",
		},
		{
			name:  "uncollared with manifold",
			reply: PipeSetReply{Manifold: "team1", Name: "room", TTLSeconds: 3600, MaxMsgs: 100, MaxBytes: 1048576},
			want:  "updated pipe=team1.room retention=ttl=1h0m0s,msgs=100,bytes=1048576\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			PrintPipeSet(&buf, tc.reply)
			if got := buf.String(); got != tc.want {
				t.Errorf("PrintPipeSet:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The IPC verb name follows the existing PipeCreate/PipeDestroy pattern.
func TestIPCPipeSet_VerbName(t *testing.T) {
	if IPCPipeSet != "PipeSet" {
		t.Errorf("IPCPipeSet = %q, want %q", IPCPipeSet, "PipeSet")
	}
}

// A `pipe set` naming exactly one override must not be indistinguishable
// on the wire from one naming none: nil stays absent, set stays present.
// This is what lets the server merge instead of clobber.
func TestPipeSetRequest_NilOverridesOmittedFromJSON(t *testing.T) {
	msgs := 5
	req := PipeSetRequest{Handle: "chat", Name: "ring", MaxMsgs: &msgs}
	got := string(mustMarshal(t, req))
	if !strings.Contains(got, `"max_msgs":5`) {
		t.Errorf("max_msgs missing from %s", got)
	}
	for _, absent := range []string{"ttl_seconds", "max_bytes"} {
		if strings.Contains(got, absent) {
			t.Errorf("%s should be omitted when nil, got %s", absent, got)
		}
	}
}
