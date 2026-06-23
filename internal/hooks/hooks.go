// Package hooks runs user lifecycle scripts from a mount's .devbox/hooks/ dir on
// sync events. A script runs via bash (or pwsh for a .ps1 hook) with the sync
// context injected as env vars; a pre-* hook's non-zero exit vetoes that step.
package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// DefaultTimeout bounds a hook; a hung hook is killed, never wedging the sync loop.
const DefaultTimeout = 60 * time.Second

// Event names (also the hook script filenames).
const (
	PrePush    = "pre-push"
	PostPush   = "post-push"
	PrePull    = "pre-pull"
	PostPull   = "post-pull"
	OnConflict = "on-conflict"
)

// AllEvents lists the supported hook events (also the script filenames), in
// lifecycle order. pre-* events can veto their step by exiting non-zero.
func AllEvents() []string {
	return []string{PrePush, PostPush, PrePull, PostPull, OnConflict}
}

// IsEvent reports whether name is a supported hook event.
func IsEvent(name string) bool { return slices.Contains(AllEvents(), name) }

// Dir returns a mount's hooks directory: root/.devbox/hooks.
func Dir(root string) string { return filepath.Join(root, ".devbox", "hooks") }

// Sample returns a commented, ready-to-edit bash template for an event hook,
// documenting the env vars devbox injects.
func Sample(event string) string {
	return "#!/usr/bin/env bash\n" +
		"# devbox " + event + " hook — runs on every " + event + ".\n" +
		"# Injected environment:\n" +
		"#   DEVBOX_EVENT          the event name (" + event + ")\n" +
		"#   DEVBOX_MOUNT          this mount's local root\n" +
		"#   DEVBOX_SHARE          the share name\n" +
		"#   DEVBOX_HOST           this device's hostname\n" +
		"#   DEVBOX_REMOTE         the hub URL\n" +
		"#   DEVBOX_SNAPSHOT       the snapshot id involved\n" +
		"#   DEVBOX_CHANGED_FILES  path to a newline-delimited list of changed files\n" +
		"#\n" +
		"# A non-zero exit from a pre-* hook VETOES that sync step.\n" +
		"set -euo pipefail\n\n" +
		"echo \"devbox " + event + ": $DEVBOX_SHARE @ $DEVBOX_SNAPSHOT\"\n"
}

// Runner runs hooks for one mount.
type Runner struct {
	Root    string // mount local root; hooks live at Root/.devbox/hooks/
	Share   string
	Host    string
	Remote  string
	Timeout time.Duration
	Stdout  io.Writer // hook stdout (defaults to os.Stdout)
	Stderr  io.Writer // hook stderr (defaults to os.Stderr)
}

// New returns a Runner for a mount with the default timeout.
func New(root, share, host, remote string) *Runner {
	return &Runner{Root: root, Share: share, Host: host, Remote: remote, Timeout: DefaultTimeout}
}

// Run executes the hook for event if a matching executable script exists. The
// changed file list is written to a temp file referenced by DEVBOX_CHANGED_FILES.
// Returns nil if no hook exists; otherwise the hook's error (non-nil from a pre-*
// hook means the caller should veto that step). Nil receiver is a no-op.
func (r *Runner) Run(event string, changed []string, snapshot string) error {
	if r == nil {
		return nil
	}
	script, interp, ok := r.find(event)
	if !ok {
		return nil
	}

	tmp, err := os.CreateTemp("", "devbox-changes-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if len(changed) > 0 {
		_, _ = tmp.WriteString(strings.Join(changed, "\n") + "\n")
	}
	tmp.Close()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, interp, script)
	cmd.Dir = r.Root
	cmd.Env = append(os.Environ(),
		"DEVBOX_EVENT="+event,
		"DEVBOX_MOUNT="+r.Root,
		"DEVBOX_SHARE="+r.Share,
		"DEVBOX_HOST="+r.Host,
		"DEVBOX_REMOTE="+r.Remote,
		"DEVBOX_SNAPSHOT="+snapshot,
		"DEVBOX_CHANGED_FILES="+tmp.Name(),
	)
	cmd.Stdout = orElse(r.Stdout, os.Stdout)
	cmd.Stderr = orElse(r.Stderr, os.Stderr)
	// ponytail: after a timeout kill, bound how long Wait blocks on a child that
	// inherited the output pipe (e.g. a `sleep`). Killing the whole process group
	// is the upgrade path if orphaned grandchildren become a problem.
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("hook %s timed out after %s", event, timeout)
	}
	if runErr != nil {
		return fmt.Errorf("hook %s: %w", event, runErr)
	}
	return nil
}

// find locates the hook script + interpreter: an executable <event>.ps1 (pwsh)
// or an executable <event> (bash).
func (r *Runner) find(event string) (script, interp string, ok bool) {
	dir := Dir(r.Root)
	if ps := filepath.Join(dir, event+".ps1"); executable(ps) {
		return ps, "pwsh", true
	}
	if base := filepath.Join(dir, event); executable(base) {
		return base, "bash", true
	}
	return "", "", false
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // no exec bit on Windows
	}
	return info.Mode().Perm()&0o111 != 0
}

func orElse(w, def io.Writer) io.Writer {
	if w == nil {
		return def
	}
	return w
}
