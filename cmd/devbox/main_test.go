package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/config"
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

// TestDoctorJSON proves `doctor --json` emits valid JSON and still reports
// failure (non-zero exit) on an unjoined device.
func TestDoctorJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // unjoined

	var buf bytes.Buffer
	cmd := doctorCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--json"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("doctor on an unjoined device must return an error (non-zero exit)")
	}
	var checks []struct {
		Name, Status, Detail string
	}
	if jerr := json.Unmarshal(buf.Bytes(), &checks); jerr != nil {
		t.Fatalf("doctor --json emitted invalid JSON: %v\n%s", jerr, buf.String())
	}
	if len(checks) == 0 || checks[0].Status != "fail" {
		t.Fatalf("expected a failing identity check, got %+v", checks)
	}
}

// TestConflictsRm proves `conflicts --rm` deletes only .conflict- copies.
func TestConflictsRm(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	mountDir := t.TempDir()
	keep := filepath.Join(mountDir, "keep.txt")
	conflictFile := filepath.Join(mountDir, "doc.conflict-host-123.txt")
	if err := os.WriteFile(keep, []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictFile, []byte("loser"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveDaemon(filepath.Join(cfg, "devbox"), config.Daemon{
		Hub: "http://h", Mounts: []config.Mount{{Share: "proj", Local: mountDir, Hub: "http://h"}},
	}); err != nil {
		t.Fatal(err)
	}

	cmd := conflictsCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--rm"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conflicts --rm: %v", err)
	}
	if _, err := os.Stat(conflictFile); !os.IsNotExist(err) {
		t.Error("conflict copy should have been deleted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("real file must NOT be deleted")
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

// TestClockSkewStatus covers the 30-second threshold in both directions.
func TestClockSkewStatus(t *testing.T) {
	for _, tc := range []struct {
		skew     time.Duration
		wantOK   bool
		wantWarn bool
	}{
		{0, true, false},
		{30 * time.Second, true, false},
		{31 * time.Second, false, true},
		{-45 * time.Second, false, true},
	} {
		ok, warn := clockSkewStatus(tc.skew)
		if ok != tc.wantOK || warn != tc.wantWarn {
			t.Errorf("clockSkewStatus(%v) = (%v,%v), want (%v,%v)", tc.skew, ok, warn, tc.wantOK, tc.wantWarn)
		}
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
