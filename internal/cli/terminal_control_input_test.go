package cli

import (
	"bytes"
	"testing"
)

// Ctrl-C must reach the remote (to interrupt its foreground process) and must
// NOT detach the control session — the reported bug was that it did both.
func TestControlInputAction_CtrlCForwardedNotDetach(t *testing.T) {
	forward, detach := controlInputAction([]byte{0x03}, controlDetachKey)
	if detach {
		t.Fatalf("Ctrl-C detached the session; want forwarded + stay attached")
	}
	if !bytes.Equal(forward, []byte{0x03}) {
		t.Fatalf("Ctrl-C forward = %v, want [3] (passed through to remote)", forward)
	}
}

// Ctrl-D detaches locally and is NOT forwarded (so the remote shell doesn't get
// an EOF on detach).
func TestControlInputAction_CtrlDDetachesLocally(t *testing.T) {
	forward, detach := controlInputAction([]byte{controlDetachKey}, controlDetachKey)
	if !detach {
		t.Fatalf("Ctrl-D did not detach; want detach")
	}
	if len(forward) != 0 {
		t.Fatalf("Ctrl-D forwarded %v to remote; want nothing (local detach)", forward)
	}
}

// Bytes typed before the detach key in the same read are still forwarded.
func TestControlInputAction_ForwardsPrefixBeforeDetach(t *testing.T) {
	forward, detach := controlInputAction([]byte("ls"+string(controlDetachKey)), controlDetachKey)
	if !detach {
		t.Fatalf("want detach on trailing Ctrl-D")
	}
	if string(forward) != "ls" {
		t.Fatalf("forward = %q, want \"ls\"", forward)
	}
}

// Ordinary keystrokes forward verbatim and never detach.
func TestControlInputAction_PlainKeysForwarded(t *testing.T) {
	forward, detach := controlInputAction([]byte("echo hi\r"), controlDetachKey)
	if detach {
		t.Fatalf("plain keys must not detach")
	}
	if string(forward) != "echo hi\r" {
		t.Fatalf("forward = %q, want verbatim", forward)
	}
}
