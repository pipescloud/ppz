//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pipescloud/ppz/internal/cliproto"
)

func TestForwardStdinWritesPayloadVerbatim(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ppz-forward-stdin-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "daemon.sock")
	t.Setenv("PPZ_IPC_SOCKET", sock)

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"no newline", "hello"},
		{"escape sequence", "\x1b[13u"},
		{"already has newline", "hello\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(sock)
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(conn).Decode(&req)
				_ = json.NewEncoder(conn).Encode(cliproto.ReadEvent{
					Message: &cliproto.ReadMessage{Payload: tc.payload},
				})
			}()

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer r.Close()
			defer w.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			// No lease held (fresh leaseState → "" holder), so enforcement
			// is inert and every message forwards verbatim, as before.
			go forwardStdin(ctx, "myhost", w, newLeaseState())

			buf := make([]byte, 64)
			_ = r.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := r.Read(buf)
			got := string(buf[:n])

			if got != tc.payload {
				t.Errorf("forwardStdin wrote %q, want %q (verbatim — no appended newline)", got, tc.payload)
			}
		})
	}
}
