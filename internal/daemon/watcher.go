package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// watchState polls $PPZ_HOME/{credentials,current} for mtime/existence
// changes and reloads when they shift. SIGHUP via hupCh forces a reload too.
// Drops the NATS connection when credentials disappear so the next IPC call
// reconnects cleanly with fresh creds (or returns ENotLoggedIn).
func (d *Daemon) watchState(ctx context.Context, hupCh <-chan os.Signal) {
	credPath := filepath.Join(d.Home, fileCredentials)
	curPath := filepath.Join(d.Home, fileCurrent)

	var lastCred, lastCur fileSig
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			// fall through to reload
		case <-tick.C:
			c := stat(credPath)
			u := stat(curPath)
			if c == lastCred && u == lastCur {
				continue
			}
			lastCred, lastCur = c, u
		}
		_ = d.State.LoadFromDisk()
		if _, ok := d.State.Credentials(); !ok {
			// Logout / creds-deleted-out-of-band crosses an account
			// boundary the same way cross-account login does (see
			// handleLogin) — the cached rows are the logged-out
			// account's agents and must not keep rendering in
			// `ppz who`. Cleared outside the NC guard so a daemon
			// that never got NATS up still forgets them; idempotent,
			// so re-running on later logged-out reloads is free.
			d.Heartbeats.Clear()
			if d.NC != nil {
				// Drop NC and evict every live follow conn so the CLI
				// sees EOF on its IPC socket and redials. Without the
				// follow eviction the stdin/inbox forwarders sit on a
				// healthy-looking conn whose underlying JetStream
				// consumer just died.
				d.swapNC("watchState-creds-gone", nil)
			}
		}
		// Capture sigs after LoadFromDisk so hup-triggered reloads also
		// align the cache.
		lastCred = stat(credPath)
		lastCur = stat(curPath)
	}
}

type fileSig struct {
	exists bool
	mtime  int64
	size   int64
}

func stat(path string) fileSig {
	st, err := os.Stat(path)
	if err != nil {
		return fileSig{}
	}
	return fileSig{exists: true, mtime: st.ModTime().UnixNano(), size: st.Size()}
}
