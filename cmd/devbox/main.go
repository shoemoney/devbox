// Command devbox is the device-side CLI + daemon entrypoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/daemon"
	"git.shoemoney.ai/shoemoney/devbox/internal/identity"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/syncer"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
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
		restoreCmd(),
		startCmd(),
		statusCmd(),
		versionCmd(),
		stub("stop", "⏹️  stop the sync daemon", "M7"),
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
				fmt.Fprintf(out, "%s  %s  %s\n", short(s.ID), ts, s.Device)
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

// findMount returns the first configured mount for share.
func findMount(mounts []config.Mount, share string) (config.Mount, bool) {
	for _, m := range mounts {
		if m.Share == share {
			return m, true
		}
	}
	return config.Mount{}, false
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

func stub(use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%q is not implemented yet (arrives in %s)", use, milestone)
		},
	}
}
