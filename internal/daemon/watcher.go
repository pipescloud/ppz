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
		// A failed reload is a transient read problem (EACCES, EMFILE,
		// EIO), NOT a logout — LoadFromDisk keeps the prior in-memory
		// state on error, and acting on it here would destructively
		// drop NATS and wipe the who-cache for a user who is genuinely
		// logged in. Skip and retry on the next tick/HUP.
		if err := d.State.LoadFromDisk(); err != nil {
			// Zero the sigs so the next tick unconditionally retries —
			// the sig cache was already advanced past the change that
			// triggered this reload.
			lastCred, lastCur = fileSig{}, fileSig{}
			continue
		}
		if _, ok := d.State.Credentials(); !ok {
			if d.NC != nil {
				// Logout / creds-deleted-out-of-band: drop NC and evict
				// every live follow conn so the CLI sees EOF on its IPC
				// socket and redials. Without the follow eviction the
				// stdin/inbox forwarders sit on a healthy-looking conn
				// whose underlying JetStream consumer just died.
				d.swapNC("watchState-creds-gone", nil)
			}
			// Forget the logged-out account's rows AFTER the swap (per
			// the ordering handleLogin documents), outside the NC guard
			// so a daemon that never got NATS up still forgets them;
			// idempotent, so re-running on later logged-out reloads is
			// free. Rendering-wise this is hygiene: handleWho scopes to
			// the current account, and logged out there is none.
			d.Heartbeats.Clear()
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
