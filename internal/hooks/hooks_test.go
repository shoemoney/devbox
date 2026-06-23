package hooks

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeHook(t *testing.T, root, event, script string) {
	t.Helper()
	dir := filepath.Join(root, ".devbox", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, event), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunInjectsEnvAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out.txt")
	// Hook records its event, share, and the changed-files contents.
	writeHook(t, root, "post-pull", "#!/usr/bin/env bash\n"+
		"{ echo \"$DEVBOX_EVENT $DEVBOX_SHARE $DEVBOX_SNAPSHOT\"; cat \"$DEVBOX_CHANGED_FILES\"; } > '"+out+"'\n")

	r := New(root, "projects", "myhost", "hub.example")
	r.Stdout, r.Stderr = io.Discard, io.Discard
	if err := r.Run("post-pull", []string{"a.txt", "sub/b.go"}, "snap123"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := "post-pull projects snap123\na.txt\nsub/b.go\n"
	if got != want {
		t.Fatalf("hook output = %q, want %q", got, want)
	}
}

func TestRunAbsentHookIsNoOp(t *testing.T) {
	r := New(t.TempDir(), "s", "h", "r")
	if err := r.Run("pre-push", nil, ""); err != nil {
		t.Fatalf("absent hook should be a no-op, got %v", err)
	}
}

func TestPreHookVetoReturnsError(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "pre-push", "#!/usr/bin/env bash\nexit 3\n")
	r := New(root, "s", "h", "r")
	r.Stdout, r.Stderr = io.Discard, io.Discard
	if err := r.Run("pre-push", nil, ""); err == nil {
		t.Fatal("a non-zero pre-push hook must return an error (veto)")
	}
}

func TestRunTimeoutKillsHook(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "post-pull", "#!/usr/bin/env bash\nsleep 5\n")
	r := New(root, "s", "h", "r")
	r.Stdout, r.Stderr = io.Discard, io.Discard
	r.Timeout = 200 * time.Millisecond
	start := time.Now()
	err := r.Run("post-pull", nil, "")
	if err == nil {
		t.Fatal("a hanging hook must time out with an error")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("hook was not killed promptly on timeout (should be ~timeout + WaitDelay, not 5s)")
	}
}

func TestNilRunnerIsNoOp(t *testing.T) {
	var r *Runner
	if err := r.Run("post-pull", []string{"x"}, "s"); err != nil {
		t.Fatalf("nil runner should be a no-op, got %v", err)
	}
}
