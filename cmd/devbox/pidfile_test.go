package main

import (
	"os"
	"runtime"
	"strconv"
	"testing"
)

// TestRunningPidStartTokenMismatch covers the PID-reuse guard: a pidfile whose
// pid is alive but whose recorded start token disagrees with the live process's
// token belongs to a recycled pid, not our daemon, and must read as not-alive
// (stale). The token is only verifiable on Linux (/proc/<pid>/stat field 22);
// elsewhere processStartToken can't confirm, so we skip.
func TestRunningPidStartTokenMismatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start token is unverifiable on " + runtime.GOOS + "; falls back to bare-pid liveness")
	}
	dir := t.TempDir()

	// Sanity: our own process must yield a token on Linux.
	want, ok := processStartToken(os.Getpid())
	if !ok || want == "" {
		t.Fatalf("processStartToken(self) = (%q,%v), want a non-empty token", want, ok)
	}

	// A pidfile naming our (live) pid but a deliberately-wrong token must NOT be
	// reported as our running daemon.
	bad := []byte(strconv.Itoa(os.Getpid()) + " " + want + "deadbeef")
	if err := os.WriteFile(pidPath(dir), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := runningPid(dir); ok {
		t.Fatalf("runningPid = (%d,true) for a mismatched start token; want not-alive (stale)", pid)
	}

	// The contents writePid records for us must read as alive, proving the guard
	// rejects only the impostor. writePid reclaims the stale file written above.
	if err := writePid(dir); err != nil {
		t.Fatal(err)
	}
	if pid, ok := runningPid(dir); !ok || pid != os.Getpid() {
		t.Fatalf("runningPid = (%d,%v) for our own token; want (%d,true)", pid, ok, os.Getpid())
	}
}
