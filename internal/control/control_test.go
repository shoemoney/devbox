//go:build !windows

package control

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeDaemon is a minimal Daemon for exercising the control server in isolation.
type fakeDaemon struct {
	mu       sync.Mutex
	paused   bool
	resumes  int
	pauseFor time.Duration
}

func (f *fakeDaemon) StateSnapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return State{
		Paused: f.paused,
		Mounts: []MountState{{Share: "s", Subpath: "p", Local: "/tmp/x", ReadOnly: true, BaseSnapshot: "snap123"}},
	}
}

func (f *fakeDaemon) PauseFor(dur time.Duration) {
	f.mu.Lock()
	f.paused = true
	f.pauseFor = dur
	f.mu.Unlock()
}

func (f *fakeDaemon) Pause() {
	f.mu.Lock()
	f.paused = true
	f.mu.Unlock()
}

func (f *fakeDaemon) Resume() {
	f.mu.Lock()
	f.paused = false
	f.resumes++
	f.mu.Unlock()
}

// shortTempDir returns a short-pathed temp dir. macOS caps a Unix socket path
// (sun_path) at ~104 bytes, and t.TempDir()'s deep paths blow past that —
// so the socket lives under /tmp where the real config dir (~/.config/devbox)
// also stays comfortably short.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dbxctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveTest(t *testing.T) (string, *fakeDaemon) {
	t.Helper()
	dir := shortTempDir(t)
	fd := &fakeDaemon{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := Serve(ctx, dir, fd, t.Logf); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return dir, fd
}

// TestRoundTripPauseStateResume proves the full control loop: a fresh daemon
// reports not-paused, POST /pause flips it (visible via GET /state), and
// POST /resume clears it and triggers a catch-up.
func TestRoundTripPauseStateResume(t *testing.T) {
	dir, fd := serveTest(t)

	st, err := DialState(dir)
	if err != nil {
		t.Fatalf("DialState: %v", err)
	}
	if st.Paused {
		t.Fatal("fresh daemon should not be paused")
	}
	if len(st.Mounts) != 1 || st.Mounts[0].BaseSnapshot != "snap123" {
		t.Fatalf("state did not round-trip mounts: %+v", st.Mounts)
	}

	if err := Pause(dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st, err = DialState(dir)
	if err != nil {
		t.Fatalf("DialState after pause: %v", err)
	}
	if !st.Paused {
		t.Fatal("state should show paused after POST /pause")
	}

	if err := Resume(dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	st, err = DialState(dir)
	if err != nil {
		t.Fatalf("DialState after resume: %v", err)
	}
	if st.Paused {
		t.Fatal("state should clear paused after POST /resume")
	}
	if fd.resumes != 1 {
		t.Fatalf("Resume should have been invoked once, got %d", fd.resumes)
	}
}

// TestPauseForCarriesDuration proves POST /pause?for=<dur> reaches the daemon's
// PauseFor with the duration intact (the wire path for `devbox pause --for`).
func TestPauseForCarriesDuration(t *testing.T) {
	dir, fd := serveTest(t)

	if err := PauseFor(dir, 90*time.Minute); err != nil {
		t.Fatalf("PauseFor: %v", err)
	}
	fd.mu.Lock()
	got, paused := fd.pauseFor, fd.paused
	fd.mu.Unlock()
	if !paused {
		t.Fatal("PauseFor should have paused the daemon")
	}
	if got != 90*time.Minute {
		t.Fatalf("PauseFor duration = %s, want 90m", got)
	}
}

// TestNoDaemonReturnsNotRunning proves the client returns the typed
// ErrNotRunning when no daemon is listening, so callers can fall back.
func TestNoDaemonReturnsNotRunning(t *testing.T) {
	dir := t.TempDir() // no Serve here — socket absent

	if _, err := DialState(dir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("DialState: want ErrNotRunning, got %v", err)
	}
	if err := Pause(dir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Pause: want ErrNotRunning, got %v", err)
	}
	if err := Resume(dir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Resume: want ErrNotRunning, got %v", err)
	}
}

// TestSocketMode0600 proves the control socket is owner-only.
func TestSocketMode0600(t *testing.T) {
	dir, _ := serveTest(t)
	info, err := os.Stat(SockPath(dir))
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

// TestStaleSocketRemoved proves Serve recovers from a leftover socket file from
// a crashed daemon (else net.Listen("unix") fails with "address in use").
func TestStaleSocketRemoved(t *testing.T) {
	dir := shortTempDir(t)
	// Plant a stale socket file.
	if err := os.WriteFile(SockPath(dir), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Serve(ctx, dir, &fakeDaemon{}, t.Logf); err != nil {
		t.Fatalf("Serve over stale socket: %v", err)
	}
	if _, err := DialState(dir); err != nil {
		t.Fatalf("DialState after stale recovery: %v", err)
	}
}

// TestShutdownRemovesSocket proves ctx cancel cleans up the socket file.
func TestShutdownRemovesSocket(t *testing.T) {
	dir := shortTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := Serve(ctx, dir, &fakeDaemon{}, t.Logf); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	cancel()
	// Shutdown is async; poll briefly for the socket to disappear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(SockPath(dir)); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("socket file not removed after ctx cancel")
}
