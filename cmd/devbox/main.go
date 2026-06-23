// Command devbox is the device-side CLI + daemon entrypoint.
package main

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/daemon"
	"git.shoemoney.ai/shoemoney/devbox/internal/hooks"
	"git.shoemoney.ai/shoemoney/devbox/internal/identity"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/syncer"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
	"git.shoemoney.ai/shoemoney/devbox/internal/watch"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

var version = "0.0.0-dev"

func main() {
	root := &cobra.Command{
		Use:           "devbox",
		Short:         "📦 devbox — Dropbox for developers",
		Long:          "devbox keeps your dev directories live-synced across machines.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		joinCmd(),
		publishCmd(),
		mountCmd(),
		logCmd(),
		unmountCmd(),
		restoreCmd(),
		deployCmd(),
		hookCmd(),
		ignoreCmd(),
		conflictsCmd(),
		startCmd(),
		stopCmd(),
		statusCmd(),
		doctorCmd(),
		versionCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "devbox", version)
		},
	}
}

func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <hub> <token>",
		Short: "🎟️  enroll this device against a hub",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, token := args[0], args[1]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			id, err := identity.LoadOrCreate(dir)
			if err != nil {
				return err
			}
			name, _ := os.Hostname()

			c := transport.New(hubURL)
			resp, err := c.Join(token, name, id.Pub)
			if err != nil {
				return err
			}

			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			d.Hub, d.DeviceID, d.Bearer = hubURL, resp.DeviceID, resp.Bearer
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "device: %s\n", resp.DeviceID)
			fmt.Fprintf(out, "hub:    %s\n", hubURL)
			fmt.Fprintln(out, "✅ joined")
			return nil
		},
	}
}

func publishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish <localdir> <share>",
		Short: "📂 create a share from a local folder and push it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, share := args[0], args[1]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if d.Hub == "" || d.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}

			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			if err := c.Publish(share); err != nil {
				return err
			}
			ig, err := syncer.LoadIgnore(root)
			if err != nil {
				return err
			}
			guard, err := secret.New(nil)
			if err != nil {
				return err
			}
			head, err := c.Head(share)
			if err != nil {
				return err
			}
			res, err := syncer.Push(c, root, share, "", ig, guard, head, nil)
			if err != nil {
				return err
			}
			if res.Conflict {
				return fmt.Errorf("share %q advanced on the hub (head %s) — pull/merge lands in M3", share, short(res.Head))
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "📤 pushed %d files to %q\n", res.Files, share)
			fmt.Fprintf(out, "🧊 uploaded %d blobs (rest deduped)\n", res.Uploaded)
			if len(res.Blocked) > 0 {
				fmt.Fprintf(out, "🔐 %d secret(s) blocked from upload: %v\n", len(res.Blocked), res.Blocked)
			}
			fmt.Fprintf(out, "📸 snapshot %s\n", short(res.Snapshot))
			return nil
		},
	}
}

func mountCmd() *cobra.Command {
	var ro bool
	cmd := &cobra.Command{
		Use:   "mount <share[/subpath]> <localdir>",
		Short: "🔗 mount a hub share (or sub-path) into a local directory (clone + sync)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			share, subpath := splitShare(args[0])
			local, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if d.Hub == "" || d.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}
			if err := os.MkdirAll(local, 0o755); err != nil {
				return err
			}
			d.Mounts = upsertMount(d.Mounts, config.Mount{Share: share, Subpath: subpath, Local: local, Hub: d.Hub, ReadOnly: ro})
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}

			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			ig, err := syncer.LoadIgnore(local)
			if err != nil {
				return err
			}
			guard, err := secret.New(nil)
			if err != nil {
				return err
			}
			host, _ := os.Hostname()
			st, err := config.LoadState(dir)
			if err != nil {
				return err
			}
			key := share + "\x00" + subpath + "\x00" + local

			var pr syncer.PullResult
			var newBase string
			if ro {
				pr, err = syncer.Pull(c, local, share, subpath, st[key], host, time.Now().UnixNano(), ig, guard, nil)
				newBase = pr.Base
			} else {
				newBase, pr, err = syncer.Sync(c, local, share, subpath, st[key], host, time.Now().UnixNano(), ig, guard, nil)
			}
			if err != nil {
				return err
			}
			st[key] = newBase
			if err := config.SaveState(dir, st); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			mode := "rw"
			if ro {
				mode = "ro"
			}
			fmt.Fprintf(out, "🔗 mounted %q -> %s [%s]\n", args[0], local, mode)
			fmt.Fprintf(out, "📥 %d files, %d conflicts\n", len(pr.Written), len(pr.Conflicts))
			fmt.Fprintln(out, "▶️  run 'devbox start' to keep it live-synced")
			return nil
		},
	}
	cmd.Flags().BoolVar(&ro, "ro", false, "mount read-only (pull only, never push)")
	return cmd
}

// splitShare splits "share/sub/path" into ("share", "sub/path"); a bare "share"
// yields an empty sub-path.
func splitShare(arg string) (share, subpath string) {
	if i := strings.IndexByte(arg, '/'); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

func upsertMount(mounts []config.Mount, m config.Mount) []config.Mount {
	for i, x := range mounts {
		if x.Share == m.Share && x.Local == m.Local {
			mounts[i] = m
			return mounts
		}
	}
	return append(mounts, m)
}

func logCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log <share>",
		Short: "🕓 show a share's snapshot history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			share := args[0]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if d.Hub == "" || d.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}
			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			snaps, err := c.Log(share)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, s := range snaps {
				ts := time.Unix(s.CreatedAt, 0).Format("2006-01-02 15:04:05")
				fmt.Fprintf(out, "%s  %s  %s\n", s.ID, ts, s.Device) // full id so it round-trips to restore
			}
			return nil
		},
	}
}

func restoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <share> <snapshot> [path]",
		Short: "↩️  restore a share (or one path) to an older snapshot",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			share, snapshot := args[0], args[1]
			var onlyPath string
			if len(args) == 3 {
				onlyPath = args[2]
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if d.Hub == "" || d.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}
			m, ok := findMount(d.Mounts, share)
			if !ok {
				return fmt.Errorf("no mount for share %q — mount it first", share)
			}

			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			ig, err := syncer.LoadIgnore(m.Local)
			if err != nil {
				return err
			}
			guard, err := secret.New(nil)
			if err != nil {
				return err
			}
			host, _ := os.Hostname()
			res, err := syncer.Restore(c, m.Local, share, m.Subpath, snapshot, onlyPath, host, time.Now().UnixNano(), ig, guard, nil)
			if err != nil {
				return err
			}

			st, err := config.LoadState(dir)
			if err != nil {
				return err
			}
			st[share+"\x00"+m.Subpath+"\x00"+m.Local] = res.Snapshot
			if err := config.SaveState(dir, st); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			what := share
			if onlyPath != "" {
				what = share + "/" + onlyPath
			}
			fmt.Fprintf(out, "↩️  restored %s -> snapshot %s\n", what, short(res.Snapshot))
			return nil
		},
	}
}

func unmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmount <share>",
		Short: "⏏️  stop syncing a share's mount(s) — files stay on disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			share := args[0]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			var kept, removed []config.Mount
			for _, m := range d.Mounts {
				if m.Share == share {
					removed = append(removed, m)
				} else {
					kept = append(kept, m)
				}
			}
			if len(removed) == 0 {
				return fmt.Errorf("no mount for share %q", share)
			}
			d.Mounts = kept
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}
			st, err := config.LoadState(dir)
			if err != nil {
				return err
			}
			for _, m := range removed {
				delete(st, share+"\x00"+m.Subpath+"\x00"+m.Local)
			}
			if err := config.SaveState(dir, st); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "⏏️  unmounted %q (%d mount(s)); files left on disk\n", share, len(removed))
			return nil
		},
	}
}

func ignoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ignore <pattern>",
		Short: "🙈 append a pattern to ./.devignore (run from inside a mount)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.OpenFile(".devignore", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := fmt.Fprintln(f, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "🙈 added %q to ./.devignore (applies on next sync)\n", args[0])
			return nil
		},
	}
}

func conflictsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts",
		Short: "💥 list conflict copies across all mounts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			n := 0
			for _, m := range d.Mounts {
				_ = filepath.WalkDir(m.Local, func(p string, e fs.DirEntry, err error) error {
					if err != nil || e.IsDir() {
						return nil
					}
					if strings.Contains(e.Name(), ".conflict-") {
						rel, _ := filepath.Rel(m.Local, p)
						fmt.Fprintf(out, "  💥 %s/%s\n", m.Share, filepath.ToSlash(rel))
						n++
					}
					return nil
				})
			}
			if n == 0 {
				fmt.Fprintln(out, "✅ no conflict copies")
			} else {
				fmt.Fprintf(out, "%d conflict copy(ies) — review, then delete the loser(s)\n", n)
			}
			return nil
		},
	}
}

// findMount returns the first configured mount for share.
func findMount(mounts []config.Mount, share string) (config.Mount, bool) {
	for _, m := range mounts {
		if m.Share == share {
			return m, true
		}
	}
	return config.Mount{}, false
}

func deployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy <share> <snapshot>",
		Short: "🚀 pin a mount to a specific snapshot (apply without pushing — blue/green)",
		Long: "deploy rewrites a mount's local directory to a specific snapshot WITHOUT pushing a\n" +
			"new head — it pins the mount at that version (history untouched), so the daemon\n" +
			"won't live-advance it. Ideal for blue/green deploys of /var/www boxes. Re-mount to\n" +
			"resume live sync. Snapshot ids come from 'devbox log <share>'.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			share, snapshot := args[0], args[1]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if d.Hub == "" || d.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}
			m, ok := findDeployMount(d.Mounts, share)
			if !ok {
				return fmt.Errorf("no mount for share %q — mount it first: devbox mount %s <dir> --ro", share, share)
			}

			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			ig, err := syncer.LoadIgnore(m.Local)
			if err != nil {
				return err
			}
			guard, err := secret.New(nil)
			if err != nil {
				return err
			}
			res, err := syncer.Deploy(c, m.Local, m.Subpath, snapshot, ig, guard)
			if err != nil {
				return err
			}

			// Pin the mount so the daemon holds it at this snapshot (re-mount clears it).
			m.Pinned = true
			d.Mounts = upsertMount(d.Mounts, m)
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}
			st, err := config.LoadState(dir)
			if err != nil {
				return err
			}
			st[share+"\x00"+m.Subpath+"\x00"+m.Local] = res.Snapshot
			if err := config.SaveState(dir, st); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "🚀 deployed %s -> snapshot %s (%d files) at %s [pinned]\n", share, short(res.Snapshot), res.Written, m.Local)
			if !m.ReadOnly {
				fmt.Fprintln(out, "⚠️  this is a read-write mount; local edits here are now isolated until you re-mount")
			}
			return nil
		},
	}
}

// findDeployMount returns the read-only mount for share if one exists, else the
// first mount of any kind (deploy is intended for a --ro mount).
func findDeployMount(mounts []config.Mount, share string) (config.Mount, bool) {
	first, ok := config.Mount{}, false
	for _, m := range mounts {
		if m.Share != share {
			continue
		}
		if m.ReadOnly {
			return m, true
		}
		if !ok {
			first, ok = m, true
		}
	}
	return first, ok
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "▶️  run the sync daemon (foreground)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			cfg, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			if cfg.Hub == "" || cfg.Bearer == "" {
				return fmt.Errorf("not joined — run: devbox join <hub> <token>")
			}
			if len(cfg.Mounts) == 0 {
				return fmt.Errorf("no mounts — run: devbox mount <share> <dir>")
			}
			if err := writePid(dir); err != nil {
				return err
			}
			defer removePid(dir)
			host, _ := os.Hostname()
			dmn, err := daemon.New(dir, cfg, host, nil)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(cmd.OutOrStdout(), "▶️  devboxd watching %d mount(s); Ctrl-C to stop\n", len(cfg.Mounts))
			return dmn.Run(ctx)
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "📊 show device + sync status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			id, err := identity.Load(dir)
			if err != nil {
				fmt.Fprintln(out, "not joined yet — run: devbox join <hub> <token>")
				return nil
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return fmt.Errorf("reading config: %w", err)
			}
			fmt.Fprintf(out, "device:  %s\n", id.Fingerprint())
			fmt.Fprintf(out, "hub:     %s\n", orNone(d.Hub))
			fmt.Fprintf(out, "mounts:  %d\n", len(d.Mounts))
			for _, m := range d.Mounts {
				mode := "rw"
				if m.ReadOnly {
					mode = "ro"
				}
				if m.Pinned {
					mode += ",pinned"
				}
				fmt.Fprintf(out, "  - %s/%s -> %s [%s]\n", m.Share, m.Subpath, m.Local, mode)
			}
			return nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "⏹️  stop the running sync daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			pid, ok := runningPid(dir)
			if !ok {
				removePid(dir) // clean up a stale pidfile if present
				return fmt.Errorf("no running daemon")
			}
			p, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			if err := p.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("signaling daemon (pid %d): %w", pid, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "⏹️  sent stop to daemon (pid %d)\n", pid)
			return nil
		},
	}
}

// --- pidfile: lets `devbox stop` find a running `devbox start` ---

func pidPath(dir string) string { return filepath.Join(dir, "daemon.pid") }

// writePid atomically claims the pidfile via O_EXCL so two `devbox start`
// invocations can't both think they're the only daemon. If the file already
// exists it arbitrates: a live pid means refuse; a stale pid (crashed daemon)
// is removed and the claim retried.
func writePid(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for {
		f, err := os.OpenFile(pidPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			return werr
		}
		if !os.IsExist(err) {
			return err
		}
		if pid, ok := runningPid(dir); ok {
			return fmt.Errorf("daemon already running (pid %d) — run: devbox stop", pid)
		}
		if rerr := os.Remove(pidPath(dir)); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		// Stale pidfile removed; loop retries the exclusive create.
	}
}

func removePid(dir string) { _ = os.Remove(pidPath(dir)) }

// runningPid reports the pid in the pidfile, and whether that process is alive.
// A stale pidfile (process gone) returns ok=false.
func runningPid(dir string) (int, bool) {
	b, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	// On unix signal 0 tests liveness without delivering a signal.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "🩺 diagnose this device's devbox setup (config, hub, mounts, watcher)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			var failed bool
			check := func(ok bool, warn bool, name, detail string) {
				glyph := "✅"
				if !ok {
					if warn {
						glyph = "⚠️ "
					} else {
						glyph = "❌"
						failed = true
					}
				}
				if detail != "" {
					fmt.Fprintf(out, "%s %s — %s\n", glyph, name, detail)
				} else {
					fmt.Fprintf(out, "%s %s\n", glyph, name)
				}
			}

			fmt.Fprintf(out, "devbox %s · %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
			fmt.Fprintf(out, "config: %s\n\n", dir)

			// Identity + join state.
			if _, err := identity.Load(dir); err != nil {
				check(false, false, "identity", "not joined — run: devbox join <hub> <token>")
				return fmt.Errorf("device is not joined")
			}
			check(true, false, "identity", "device keypair present")

			d, err := config.LoadDaemon(dir)
			if err != nil {
				check(false, false, "config", err.Error())
				return err
			}
			check(d.Hub != "", false, "hub configured", d.Hub)
			check(d.Bearer != "", false, "bearer token", "present")

			// Hub reachability (unauth) + bearer validity (authed).
			if d.Hub != "" {
				hc := &http.Client{Timeout: 5 * time.Second}
				resp, err := hc.Get(strings.TrimRight(d.Hub, "/") + proto.PathMetrics)
				if err != nil {
					check(false, false, "hub reachable", err.Error())
				} else {
					resp.Body.Close()
					check(true, false, "hub reachable", d.Hub+proto.PathMetrics)
					c := transport.New(d.Hub)
					c.SetBearer(d.Bearer)
					if _, err := c.Have(nil); err != nil {
						check(false, false, "bearer accepted", err.Error())
					} else {
						check(true, false, "bearer accepted", "hub authorized this device")
					}
				}
			}

			// bash — required for lifecycle hooks.
			if _, err := exec.LookPath("bash"); err != nil {
				check(false, true, "bash", "not found — lifecycle hooks won't run")
			} else {
				check(true, false, "bash", "available for hooks")
			}

			// fsnotify watcher (catches inotify exhaustion on Linux).
			probe := filepath.Join(os.TempDir(), "devbox-doctor-watch")
			_ = os.MkdirAll(probe, 0o755)
			if w, err := watch.New(probe, 100*time.Millisecond); err != nil {
				check(false, false, "file watcher", err.Error())
			} else {
				w.Close()
				check(true, false, "file watcher", "fsnotify ok")
			}
			_ = os.RemoveAll(probe)

			// Mounts: dir exists + writable.
			check(len(d.Mounts) > 0, len(d.Mounts) == 0, "mounts", fmt.Sprintf("%d configured", len(d.Mounts)))
			for _, m := range d.Mounts {
				label := fmt.Sprintf("mount %s -> %s", m.Share, m.Local)
				if writable(m.Local) {
					mode := "rw"
					if m.ReadOnly {
						mode = "ro"
					}
					if m.Pinned {
						mode += ",pinned"
					}
					check(true, false, label, mode)
				} else {
					check(false, false, label, "local dir missing or not writable")
				}
			}

			fmt.Fprintln(out)
			if failed {
				return fmt.Errorf("doctor found problems (see ❌ above)")
			}
			fmt.Fprintln(out, "🎉 all checks passed")
			return nil
		},
	}
}

// writable reports whether dir exists and we can create a file in it.
func writable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".devbox-doctor-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

func hookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "🪝 manage a mount's lifecycle hooks",
	}
	cmd.AddCommand(hookEditCmd(), hookListCmd())
	return cmd
}

func hookEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <share> <event>",
		Short: "✏️  create/edit a hook script (events: " + strings.Join(hooks.AllEvents(), ", ") + ")",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			share, event := args[0], args[1]
			if !hooks.IsEvent(event) {
				return fmt.Errorf("unknown event %q (events: %s)", event, strings.Join(hooks.AllEvents(), ", "))
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			m, ok := findMount(d.Mounts, share)
			if !ok {
				return fmt.Errorf("no mount for share %q — mount it first", share)
			}
			hookDir := hooks.Dir(m.Local)
			if err := os.MkdirAll(hookDir, 0o755); err != nil {
				return err
			}
			path := filepath.Join(hookDir, event)
			out := cmd.OutOrStdout()
			if _, err := os.Stat(path); os.IsNotExist(err) {
				if err := os.WriteFile(path, []byte(hooks.Sample(event)), 0o755); err != nil {
					return err
				}
				fmt.Fprintf(out, "🆕 created %s\n", path)
			}
			_ = os.Chmod(path, 0o755) // ensure executable so the runner picks it up

			editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
			if editor == "" {
				fmt.Fprintf(out, "✏️  edit it: %s (set $EDITOR to open automatically)\n", path)
				return nil
			}
			ed := exec.Command(editor, path)
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
			return ed.Run()
		},
	}
}

func hookListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <share>",
		Short: "📋 list installed + available hooks for a share's mount",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			share := args[0]
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			m, ok := findMount(d.Mounts, share)
			if !ok {
				return fmt.Errorf("no mount for share %q", share)
			}
			out := cmd.OutOrStdout()
			hookDir := hooks.Dir(m.Local)
			fmt.Fprintf(out, "hooks dir: %s\n", hookDir)
			for _, e := range hooks.AllEvents() {
				glyph := "·"
				if hooks.Executable(filepath.Join(hookDir, e)) || hooks.Executable(filepath.Join(hookDir, e+".ps1")) {
					glyph = "✅"
				}
				fmt.Fprintf(out, "  %s %s\n", glyph, e)
			}
			return nil
		},
	}
}
