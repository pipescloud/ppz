package cli

import (
	"io"
	"testing"

	"github.com/pipescloud/ppz/internal/cliproto"
)

// v0.51.0: --priority flag on `ppz send`. Accepts 1|2|3 and the named
// aliases high|medium|med|low; anything else is a usage error rejected
// at the CLI before any IPC call. Unset forwards Priority=0.

func TestParsePriority(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false}, // unset
		{"1", 1, false},
		{"2", 2, false},
		{"3", 3, false},
		{"high", 1, false},
		{"medium", 2, false},
		{"med", 2, false},
		{"low", 3, false},
		{"HIGH", 1, false}, // case-insensitive aliases
		{"0", 0, true},     // explicit 0 is not a tier; only omission means unset
		{"4", 0, true},
		{"7", 0, true},
		{"-1", 0, true},
		{"urgent", 0, true},
	}
	for _, c := range cases {
		got, err := parsePriority(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePriority(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePriority(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parsePriority(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCmdSend_ForwardsPriority(t *testing.T) {
	requests, _ := setupV25SendDaemon(t, "alpha")

	if err := cmdSendForTest([]string{"foo", "hi", "--priority", "1"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("send: %v", err)
	}
	var got *cliproto.SendRequest
	for _, r := range requests.snapshot() {
		if r.Payload == "hi" {
			got = &r
			break
		}
	}
	if got == nil {
		t.Fatalf("no SendRequest reached the daemon: %+v", requests.snapshot())
	}
	if got.Priority != 1 {
		t.Fatalf("SendRequest.Priority = %d, want 1", got.Priority)
	}
}

func TestCmdSend_PriorityAliases(t *testing.T) {
	cases := []struct {
		alias string
		want  int
	}{
		{"high", 1},
		{"medium", 2},
		{"med", 2},
		{"low", 3},
	}
	for _, c := range cases {
		t.Run(c.alias, func(t *testing.T) {
			requests, _ := setupV25SendDaemon(t, "alpha")
			if err := cmdSendForTest([]string{"foo", "hi", "--priority", c.alias}, io.Discard, io.Discard); err != nil {
				t.Fatalf("send --priority %s: %v", c.alias, err)
			}
			got := requests.at(0)
			if got.Priority != c.want {
				t.Fatalf("SendRequest.Priority = %d, want %d for alias %q", got.Priority, c.want, c.alias)
			}
		})
	}
}

// --priority takes a value, so splitSendArgs MUST list it in valueFlags —
// otherwise `ppz send --priority 1 foo hi` eats "1" as the target.
func TestCmdSend_PriorityBeforePositionals(t *testing.T) {
	requests, _ := setupV25SendDaemon(t, "alpha")

	if err := cmdSendForTest([]string{"--priority", "1", "foo", "hi"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("send with leading --priority: %v", err)
	}
	got := requests.at(0)
	if got.Handle != "foo" || got.Payload != "hi" {
		t.Fatalf("positionals mangled: handle=%q payload=%q, want foo / hi (valueFlags missing --priority?)",
			got.Handle, got.Payload)
	}
	if got.Priority != 1 {
		t.Fatalf("SendRequest.Priority = %d, want 1", got.Priority)
	}
}

func TestCmdSend_NoPriorityForwardsZero(t *testing.T) {
	requests, _ := setupV25SendDaemon(t, "alpha")

	if err := cmdSendForTest([]string{"foo", "hi"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := requests.at(0)
	if got.Priority != 0 {
		t.Fatalf("SendRequest.Priority = %d, want 0 (unset)", got.Priority)
	}
}

// Invalid values are rejected at the CLI with E_INVALID_PRIORITY before
// any IPCSend reaches the daemon.
func TestCmdSend_RejectsInvalidPriorityAtCLI(t *testing.T) {
	for _, bad := range []string{"7", "0", "urgent"} {
		t.Run(bad, func(t *testing.T) {
			requests, _ := setupV25SendDaemon(t, "alpha")
			err := cmdSendForTest([]string{"foo", "hi", "--priority", bad}, io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("--priority %s should be rejected", bad)
			}
			cerr := asCliErr(t, err)
			if cerr.Code != cliproto.EInvalidPriority {
				t.Fatalf("error code = %s, want E_INVALID_PRIORITY", cerr.Code)
			}
			for _, r := range requests.snapshot() {
				if r.Payload == "hi" {
					t.Fatalf("CLI should reject before any IPCSend call; got %+v", r)
				}
			}
		})
	}
}
