// Package control is the daemon's local control plane: an HTTP/1.1 server the
// running daemon hosts on a Unix domain socket (Docker's pattern — routing and
// JSON for free), plus a thin client the CLI uses to introspect and steer it.
//
// It is what finally lets `devbox status` see the LIVE daemon (not just disk)
// and wires `devbox pause`/`resume` through to the sync loop. Everything is
// loopback-only and mode-0600: no auth, the filesystem is the boundary.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// SockName is the control socket's filename inside the devbox config dir.
const SockName = "control.sock"

// SockPath returns the control socket path for a config dir.
func SockPath(dir string) string { return filepath.Join(dir, SockName) }

// State is the running daemon's live snapshot returned by GET /state.
type State struct {
	Paused bool         `json:"paused"`
	Mounts []MountState `json:"mounts"`
}

// MountState is one mount's live view: its config plus the snapshot the daemon
// has actually applied (base_snapshot), which disk-only status can't know.
type MountState struct {
	Share        string `json:"share"`
	Subpath      string `json:"subpath"`
	Local        string `json:"local"`
	ReadOnly     bool   `json:"readonly"`
	Pinned       bool   `json:"pinned"`
	BaseSnapshot string `json:"base_snapshot"`
}

// ErrNotRunning is returned by the client helpers when the control socket is
// absent or unreachable — i.e. the daemon isn't running — so callers can fall
// back to disk-based behaviour instead of treating it as a hard error.
var ErrNotRunning = errors.New("devbox daemon not running (no control socket)")

// httpClient builds an http.Client that dials the Unix socket at dir. The host
// in request URLs is ignored by the dialer, so we use a placeholder.
func httpClient(dir string) *http.Client {
	sock := SockPath(dir)
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// notRunning maps a dial failure (socket missing / nobody listening) to
// ErrNotRunning, leaving genuine protocol errors as-is.
func notRunning(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return ErrNotRunning
	}
	return err
}

// DialState fetches the live daemon state. Returns ErrNotRunning if no daemon
// is listening on the socket.
func DialState(dir string) (State, error) {
	var st State
	resp, err := httpClient(dir).Get("http://unix/state")
	if err != nil {
		return st, notRunning(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("control /state: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// Pause asks the daemon to stop syncing until resumed.
func Pause(dir string) error { return post(dir, "/pause") }

// Resume clears the pause and triggers an immediate catch-up on all mounts.
func Resume(dir string) error { return post(dir, "/resume") }

func post(dir, path string) error {
	resp, err := httpClient(dir).Post("http://unix"+path, "", nil)
	if err != nil {
		return notRunning(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control %s: %s", path, resp.Status)
	}
	return nil
}
