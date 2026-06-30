package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestStatusJSON proves `status --json` emits valid JSON and reports not-joined
// on a fresh config dir (the scriptable path for fleet monitoring).
func TestStatusJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // empty → unjoined device

	var buf bytes.Buffer
	cmd := statusCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var got statusJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("status --json emitted invalid JSON: %v\n%s", err, buf.String())
	}
	if got.Joined {
		t.Fatalf("fresh config dir should report joined=false, got %+v", got)
	}
}

// TestConflictsJSON proves `conflicts --json` emits a JSON array (never null).
func TestConflictsJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	cmd := conflictsCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conflicts --json: %v", err)
	}
	var got []struct {
		Share string `json:"share"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("conflicts --json emitted invalid JSON: %v\n%s", err, buf.String())
	}
	if len(got) != 0 {
		t.Fatalf("no mounts should yield empty list, got %v", got)
	}
}

// TestWritePidArbitration covers the O_EXCL claim, the live-daemon refusal, and
// stale-pidfile recovery.
func TestWritePidArbitration(t *testing.T) {
	dir := t.TempDir()

	// First claim succeeds and records our (live) pid.
	if err := writePid(dir); err != nil {
		t.Fatalf("first writePid: %v", err)
	}
	if pid, ok := runningPid(dir); !ok || pid != os.Getpid() {
		t.Fatalf("runningPid = (%d,%v), want (%d,true)", pid, ok, os.Getpid())
	}

	// A second claim must refuse: the pid in the file (us) is alive.
	if err := writePid(dir); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second writePid = %v, want 'already running'", err)
	}

	// A stale pidfile (a pid that isn't running) is reclaimed, not refused.
	dead := 0x7fffffff // implausibly-high pid; Signal(0) returns ESRCH
	if err := os.WriteFile(pidPath(dir), []byte(strconv.Itoa(dead)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := runningPid(dir); ok {
		t.Fatal("runningPid reported a dead pid as alive")
	}
	if err := writePid(dir); err != nil {
		t.Fatalf("reclaiming a stale pidfile: %v", err)
	}
	if pid, _ := runningPid(dir); pid != os.Getpid() {
		t.Fatalf("after reclaim pid = %d, want %d", pid, os.Getpid())
	}

	// removePid clears it.
	removePid(dir)
	if _, ok := runningPid(dir); ok {
		t.Fatal("pidfile still live after removePid")
	}
}
