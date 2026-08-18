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

// Compared against PipeCreateRequest field-by-field rather than against a
// hardcoded table. The point of the test is that the two stay the same
// shape, so create's shape has to be the thing it reads — a pinned copy
// of today's fields would keep passing after create moved, and "mirrors
// create" would quietly stop being true.
func TestPipeSetRequest_MirrorsCreateShape(t *testing.T) {
	set := reflect.TypeOf(PipeSetRequest{})
	create := reflect.TypeOf(PipeCreateRequest{})

	if set.NumField() != create.NumField() {
		t.Fatalf("field count: PipeSetRequest has %d, PipeCreateRequest has %d — the two addressing shapes have diverged",
			set.NumField(), create.NumField())
	}
	for i := range create.NumField() {
		want := create.Field(i)
		got, ok := set.FieldByName(want.Name)
		if !ok {
			t.Errorf("PipeSetRequest is missing %s, which PipeCreateRequest has", want.Name)
			continue
		}
		if got.Type != want.Type {
			t.Errorf("PipeSetRequest.%s type = %v, want %v (as on PipeCreateRequest)", want.Name, got.Type, want.Type)
		}
		if got.Tag.Get("json") != want.Tag.Get("json") {
			t.Errorf("PipeSetRequest.%s json tag = %q, want %q (as on PipeCreateRequest)",
				want.Name, got.Tag.Get("json"), want.Tag.Get("json"))
		}
	}

	// Anchor the shared shape to concrete wire names, so a change made to
	// BOTH structs at once still has to be deliberate.
	for name, tag := range map[string]string{
		"Handle":     "handle",
		"Name":       "name",
		"TTLSeconds": "ttl_seconds,omitempty",
		"MaxMsgs":    "max_msgs,omitempty",
		"MaxBytes":   "max_bytes,omitempty",
	} {
		f, ok := set.FieldByName(name)
		if !ok {
			t.Errorf("PipeSetRequest.%s missing", name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("PipeSetRequest.%s json tag = %q, want %q", name, got, tag)
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
