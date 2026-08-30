package cliproto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// `ppz ls` gains a NAMESPACE column as the leftmost column. It carries
// the per-row manifold (the namespace the pipe lives in); empty/root
// renders as "-", matching the table's missing-value convention used
// by LAST and PAYLOAD.
//
// PIPE column drops the manifold prefix once NAMESPACE owns that info:
//
//	NAMESPACE  PIPE             UNREAD  BUFFERED  LAST  PAYLOAD  CREATOR
//	team1      chat.broadcast   1       1         …     hello    foo
//	-          chat.broadcast   0       0         -     -        foo
//	team1      room             3       3         …     ping     foo
//	-          room             0       0         -     -        foo
//
// JSON mirrors the table: every row carries a `namespace` field
// (empty string at root, manifold path otherwise). The previous
// uncollared-only `manifold` key is unified under `namespace` so
// callers don't have to special-case the two row shapes.

// --- Table tests ---------------------------------------------------------

func TestPrintList_HeaderHasNamespaceFirst(t *testing.T) {
	withFrozenNow(t, fixedNow())
	at := fixedNow().Add(-5 * time.Minute)

	var buf bytes.Buffer
	PrintList(&buf, []Source{sampleSourceFooChat(at)}, false)

	header := strings.SplitN(buf.String(), "\n", 2)[0]
	fields := strings.Fields(header)
	wantOrder := []string{"NAMESPACE", "PIPE", "UNREAD", "BUFFERED", "LAST", "PAYLOAD", "CREATOR"}
	if len(fields) != len(wantOrder) {
		t.Fatalf("header field count = %d (%v), want %d (%v)", len(fields), fields, len(wantOrder), wantOrder)
	}
	for i, want := range wantOrder {
		if fields[i] != want {
			t.Errorf("header field[%d] = %q, want %q (full header: %q)", i, fields[i], want, header)
		}
	}
}

func TestPrintList_CollaredRowAtRootShowsDashNamespace(t *testing.T) {
	withFrozenNow(t, fixedNow())
	at := fixedNow().Add(-5 * time.Minute)
	src := Source{
		Handle: "chat", CreatedBy: "foo",
		PipeInfos: []PipeInfo{
			{Pipe: "inbox", Total: 1, Unread: 1, LastAt: &at, Preview: "hi"},
		},
	}
	var buf bytes.Buffer
	PrintList(&buf, []Source{src}, false)

	row := dataRowContaining(t, buf.String(), "chat.inbox")
	if got, want := firstField(row), "-"; got != want {
		t.Errorf("collared row at root: NAMESPACE = %q, want %q (row: %q)", got, want, row)
	}
}

func TestPrintList_CollaredRowInManifoldShowsManifold(t *testing.T) {
	withFrozenNow(t, fixedNow())
	at := fixedNow().Add(-5 * time.Minute)
	src := Source{
		Handle: "chat", Manifold: "team1", CreatedBy: "foo",
		PipeInfos: []PipeInfo{
			{Pipe: "inbox", Total: 1, Unread: 1, LastAt: &at, Preview: "hi"},
		},
	}
	var buf bytes.Buffer
	PrintList(&buf, []Source{src}, false)

	row := dataRowContaining(t, buf.String(), "chat.inbox")
	if got, want := firstField(row), "team1"; got != want {
		t.Errorf("collared row at manifold: NAMESPACE = %q, want %q (row: %q)", got, want, row)
	}
}

func TestPrintList_UncollaredRowAtRootShowsDashNamespace(t *testing.T) {
	withFrozenNow(t, fixedNow())
	uc := []UncollaredPipe{
		{Manifold: "", Name: "plaza", Info: PipeInfo{Total: 0, Unread: 0, CreatedBy: "foo"}},
	}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, nil, uc, false, false)

	row := dataRowContaining(t, buf.String(), "plaza")
	if got, want := firstField(row), "-"; got != want {
		t.Errorf("uncollared row at root: NAMESPACE = %q, want %q (row: %q)", got, want, row)
	}
}

func TestPrintList_UncollaredRowInManifoldShowsManifold(t *testing.T) {
	withFrozenNow(t, fixedNow())
	uc := []UncollaredPipe{
		{Manifold: "team1", Name: "room", Info: PipeInfo{Total: 0, Unread: 0, CreatedBy: "foo"}},
	}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, nil, uc, false, false)

	row := dataRowContaining(t, buf.String(), "room")
	if got, want := firstField(row), "team1"; got != want {
		t.Errorf("uncollared row at manifold: NAMESPACE = %q, want %q (row: %q)", got, want, row)
	}
}

// PIPE column must drop the manifold prefix once NAMESPACE owns that
// info. Pre-change a row for source `chat` in manifold `team1` rendered
// the PIPE column as `team1.chat.inbox`; post-change PIPE shows
// `chat.inbox` and NAMESPACE shows `team1`.
func TestPrintList_PipeColumnDropsManifoldPrefix_Collared(t *testing.T) {
	withFrozenNow(t, fixedNow())
	at := fixedNow().Add(-5 * time.Minute)
	src := Source{
		Handle: "chat", Manifold: "team1", CreatedBy: "foo",
		PipeInfos: []PipeInfo{
			{Pipe: "inbox", Total: 1, Unread: 1, LastAt: &at, Preview: "hi"},
		},
	}
	var buf bytes.Buffer
	PrintList(&buf, []Source{src}, false)

	out := buf.String()
	if strings.Contains(out, "team1.chat.inbox") {
		t.Errorf("PIPE column must drop the manifold prefix once NAMESPACE owns it; found legacy 'team1.chat.inbox' in:\n%s", out)
	}
	row := dataRowContaining(t, out, "chat.inbox")
	if got, want := fieldAt(row, 1), "chat.inbox"; got != want {
		t.Errorf("PIPE field = %q, want %q (row: %q)", got, want, row)
	}
}

func TestPrintList_PipeColumnDropsManifoldPrefix_Uncollared(t *testing.T) {
	withFrozenNow(t, fixedNow())
	uc := []UncollaredPipe{
		{Manifold: "team1", Name: "room", Info: PipeInfo{Total: 0, Unread: 0, CreatedBy: "foo"}},
	}
	var buf bytes.Buffer
	PrintListWithUncollared(&buf, nil, uc, false, false)

	out := buf.String()
	if strings.Contains(out, "team1.room") {
		t.Errorf("PIPE column must drop the manifold prefix once NAMESPACE owns it; found legacy 'team1.room' in:\n%s", out)
	}
	row := dataRowContaining(t, out, "room")
	if got, want := fieldAt(row, 1), "room"; got != want {
		t.Errorf("PIPE field = %q, want %q (row: %q)", got, want, row)
	}
}

// --- JSON tests ----------------------------------------------------------

func TestPrintListJSON_CollaredRowCarriesNamespace(t *testing.T) {
	at := fixedNow().Add(-5 * time.Minute)
	sources := []Source{
		{
			Handle: "chat", Manifold: "team1", CreatedBy: "foo",
			PipeInfos: []PipeInfo{
				{Pipe: "inbox", Total: 1, Unread: 1, LastAt: &at, Payload: "hi"},
			},
		},
		{
			Handle: "lobby", Manifold: "", CreatedBy: "foo",
			PipeInfos: []PipeInfo{
				{Pipe: "inbox", Total: 0, Unread: 0},
			},
		},
	}
	var buf bytes.Buffer
	PrintListJSON(&buf, sources)

	wantNamespace := map[string]string{
		"chat":  "team1",
		"lobby": "",
	}
	rows := decodeJSONLines(t, buf.String())
	if len(rows) != 2 {
		t.Fatalf("expected 2 JSON rows, got %d", len(rows))
	}
	for _, row := range rows {
		handle, _ := row["handle"].(string)
		want, ok := wantNamespace[handle]
		if !ok {
			t.Errorf("unexpected handle %q in row %v", handle, row)
			continue
		}
		got, present := row["namespace"]
		if !present {
			t.Errorf("handle=%q row missing `namespace` key; row=%v", handle, row)
			continue
		}
		gotStr, _ := got.(string)
		if gotStr != want {
			t.Errorf("handle=%q: namespace=%q, want %q", handle, gotStr, want)
		}
	}
}

func TestPrintListJSON_UncollaredRowCarriesNamespace(t *testing.T) {
	uc := []UncollaredPipe{
		{Manifold: "team1", Name: "room", Info: PipeInfo{Total: 0, Unread: 0, CreatedBy: "foo"}},
		{Manifold: "", Name: "plaza", Info: PipeInfo{Total: 0, Unread: 0, CreatedBy: "foo"}},
	}
	var buf bytes.Buffer
	PrintListJSONWithUncollared(&buf, nil, uc)

	wantNamespace := map[string]string{
		"room":  "team1",
		"plaza": "",
	}
	rows := decodeJSONLines(t, buf.String())
	if len(rows) != 2 {
		t.Fatalf("expected 2 JSON rows, got %d", len(rows))
	}
	for _, row := range rows {
		pipe, _ := row["pipe"].(string)
		want, ok := wantNamespace[pipe]
		if !ok {
			t.Errorf("unexpected pipe %q in row %v", pipe, row)
			continue
		}
		got, present := row["namespace"]
		if !present {
			t.Errorf("pipe=%q uncollared row missing `namespace` key; row=%v", pipe, row)
			continue
		}
		gotStr, _ := got.(string)
		if gotStr != want {
			t.Errorf("pipe=%q: namespace=%q, want %q", pipe, gotStr, want)
		}
	}
}

// --- helpers -------------------------------------------------------------

// dataRowContaining returns the first non-header data row that includes
// `needle` as a whitespace-separated field. Fails the test if no row
// matches — keeps caller assertions tight.
func dataRowContaining(t *testing.T, table, needle string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "NAMESPACE" {
			continue // header
		}
		for _, f := range fields {
			if f == needle {
				return line
			}
		}
	}
	t.Fatalf("no data row containing %q found in:\n%s", needle, table)
	return ""
}

func firstField(row string) string {
	fields := strings.Fields(row)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func fieldAt(row string, i int) string {
	fields := strings.Fields(row)
	if i < 0 || i >= len(fields) {
		return ""
	}
	return fields[i]
}

func decodeJSONLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("non-json output line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}
