// Command devbox is the device-side CLI + daemon entrypoint.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shoemoney/devbox/internal/config"
	"github.com/shoemoney/devbox/internal/control"
	"github.com/shoemoney/devbox/internal/daemon"
	"github.com/shoemoney/devbox/internal/hooks"
	"github.com/shoemoney/devbox/internal/identity"
	"github.com/shoemoney/devbox/internal/ignore"
	"github.com/shoemoney/devbox/internal/secret"
	"github.com/shoemoney/devbox/internal/syncer"
	"github.com/shoemoney/devbox/internal/transport"
	"github.com/shoemoney/devbox/internal/watch"
	"github.com/shoemoney/devbox/pkg/proto"
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
		membersCmd(),
		inviteCmd(),
		startCmd(),
		stopCmd(),
		pauseCmd(),
		resumeCmd(),
		statusCmd(),
		doctorCmd(),
		setupCmd(),
		versionCmd(),
	)
	// First run: on a bare `devbox` (no subcommand) on a terminal, offer the setup
	// wizard if this machine isn't joined yet and the user hasn't opted out.
	if len(os.Args) == 1 {
		if dir, err := config.Dir(); err == nil && maybeOfferSetup(dir) {
			return
		}
	}
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
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			resp, err := joinHub(dir, args[0], args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "device: %s\n", resp.DeviceID)
			fmt.Fprintf(out, "hub:    %s\n", args[0])
			fmt.Fprintln(out, "✅ joined")
			return nil
		},
	}
}

// joinHub enrolls this device against hubURL with token and persists the bearer
// to the daemon config. Shared by `devbox join` and the first-run setup wizard.
func joinHub(dir, hubURL, token string) (proto.JoinResponse, error) {
	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		return proto.JoinResponse{}, err
	}
	name, _ := os.Hostname()
	resp, err := transport.New(hubURL).Join(token, name, id.Pub, id.Priv)
	if err != nil {
		return proto.JoinResponse{}, err
	}
	d, err := config.LoadDaemon(dir)
	if err != nil {
		return proto.JoinResponse{}, err
	}
	d.Hub, d.DeviceID, d.Bearer = hubURL, resp.DeviceID, resp.Bearer
	if err := config.SaveDaemon(dir, d); err != nil {
		return proto.JoinResponse{}, err
	}
	return resp, nil
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
			var extraIgnore []string
			if s, err := config.LoadSettings(dir); err == nil {
				c.SetCompress(s.Transfer.Compress) // big initial upload — honor compress over WAN
				if s.Sync.IgnoreDefaults {
					extraIgnore = ignore.Defaults // don't upload node_modules/.git/… on publish
				}
			}
			if err := c.Publish(share); err != nil {
				return err
			}
			ig, err := syncer.LoadIgnoreWith(root, extraIgnore)
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
				return fmt.Errorf("share %q already has content on the hub (head %s) — mount it for two-way sync: devbox mount %s <dir>", share, short(res.Head), share)
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
	var exclude []string
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
			d.Mounts = upsertMount(d.Mounts, config.Mount{Share: share, Subpath: subpath, Local: local, Hub: d.Hub, ReadOnly: ro, Exclude: exclude})
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}

			c := transport.New(d.Hub)
			c.SetBearer(d.Bearer)
			ig, err := syncer.LoadIgnoreWith(local, exclude)
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
	cmd.Flags().StringArrayVar(&exclude, "exclude", nil, "device-local ignore pattern (gitignore syntax; repeatable) layered on .devignore")
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

// writeJSON encodes v as indented JSON to w — the shared backend for --json flags.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func logCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
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
			if asJSON {
				return writeJSON(out, snaps)
			}
			for _, s := range snaps {
				ts := time.Unix(s.CreatedAt, 0).Format("2006-01-02 15:04:05")
				fmt.Fprintf(out, "%s  %s  %s\n", s.ID, ts, s.Device) // full id so it round-trips to restore
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func inviteCmd() *cobra.Command {
	var reshare bool
	cmd := &cobra.Command{
		Use:   "invite <share> <principal> <viewer|editor|admin|owner>",
		Short: "✉️  mint an invite token granting a principal a role on a share (M8a)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			tok, err := c.Invite(args[0], args[1], args[2], reshare)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✉️  invite for %s as %s on %s:\n  %s\nthey run: devbox join %s %s\n",
				args[1], args[2], args[0], tok, d.Hub, tok)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reshare, "reshare", false, "grant the +s bit (lets them delegate)")
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <token>",
		Short: "🗑️  revoke a pending invite token before it's redeemed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if err := c.RevokeInvite(args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "🗑️  invite revoked — it can no longer be redeemed")
			return nil
		},
	})
	return cmd
}

func membersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "members <share>",
		Short: "👥 show who can access a share (M8a roles)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			resp, err := c.Members(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if resp.Legacy {
				fmt.Fprintf(out, "%s is a legacy share — every enrolled device is an implicit owner\n", args[0])
				return nil
			}
			for _, m := range resp.Members {
				s := ""
				if m.CanReshare {
					s = " +s"
				}
				fmt.Fprintf(out, "%-20s %s%s\n", m.Principal, m.Role, s)
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
			ig, err := syncer.LoadIgnoreWith(m.Local, m.Exclude)
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
	var asJSON, rm bool
	cmd := &cobra.Command{
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
			type conflict struct {
				Share string `json:"share"`
				Path  string `json:"path"`
				abs   string // absolute path on disk (for --rm); not serialized
			}
			found := []conflict{}
			for _, m := range d.Mounts {
				_ = filepath.WalkDir(m.Local, func(p string, e fs.DirEntry, err error) error {
					if err != nil || e.IsDir() {
						return nil
					}
					if strings.Contains(e.Name(), ".conflict-") {
						rel, _ := filepath.Rel(m.Local, p)
						found = append(found, conflict{Share: m.Share, Path: filepath.ToSlash(rel), abs: p})
					}
					return nil
				})
			}
			if rm {
				removed := 0
				for _, cf := range found {
					if err := os.Remove(cf.abs); err != nil {
						fmt.Fprintf(out, "  ⚠️  could not remove %s/%s: %v\n", cf.Share, cf.Path, err)
						continue
					}
					fmt.Fprintf(out, "  🗑️  removed %s/%s\n", cf.Share, cf.Path)
					removed++
				}
				fmt.Fprintf(out, "removed %d conflict copy(ies)\n", removed)
				return nil
			}
			if asJSON {
				return writeJSON(out, found)
			}
			for _, cf := range found {
				fmt.Fprintf(out, "  💥 %s/%s\n", cf.Share, cf.Path)
			}
			if len(found) == 0 {
				fmt.Fprintln(out, "✅ no conflict copies")
			} else {
				fmt.Fprintf(out, "%d conflict copy(ies) — review, then delete the loser(s) (or `devbox conflicts --rm`)\n", len(found))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&rm, "rm", false, "delete every conflict copy (review first!)")
	return cmd
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
			ig, err := syncer.LoadIgnoreWith(m.Local, m.Exclude)
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

type statusMount struct {
	Share        string `json:"share"`
	Subpath      string `json:"subpath,omitempty"`
	Local        string `json:"local"`
	ReadOnly     bool   `json:"readonly"`
	Pinned       bool   `json:"pinned"`
	BaseSnapshot string `json:"base_snapshot,omitempty"`
	LastSyncUnix int64  `json:"last_sync_unix,omitempty"`
	LastErr      string `json:"last_err,omitempty"`
}

type statusJSON struct {
	Joined bool          `json:"joined"`
	Device string        `json:"device,omitempty"`
	Hub    string        `json:"hub,omitempty"`
	Live   bool          `json:"live"`
	Paused bool          `json:"paused"`
	Mounts []statusMount `json:"mounts"`
}

func statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
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
				if asJSON {
					return writeJSON(out, statusJSON{Joined: false})
				}
				fmt.Fprintln(out, "not joined yet — run: devbox join <hub> <token>")
				return nil
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return fmt.Errorf("reading config: %w", err)
			}

			if asJSON {
				s := statusJSON{Joined: true, Device: id.Fingerprint(), Hub: d.Hub, Mounts: []statusMount{}}
				if live, err := control.DialState(dir); err == nil {
					s.Live, s.Paused = true, live.Paused
					for _, m := range live.Mounts {
						s.Mounts = append(s.Mounts, statusMount{
							Share: m.Share, Subpath: m.Subpath, Local: m.Local,
							ReadOnly: m.ReadOnly, Pinned: m.Pinned, BaseSnapshot: m.BaseSnapshot,
							LastSyncUnix: m.LastSyncUnix, LastErr: m.LastErr,
						})
					}
				} else {
					for _, m := range d.Mounts {
						s.Mounts = append(s.Mounts, statusMount{
							Share: m.Share, Subpath: m.Subpath, Local: m.Local,
							ReadOnly: m.ReadOnly, Pinned: m.Pinned,
						})
					}
				}
				return writeJSON(out, s)
			}

			// Attempt live daemon state once; used for both TTY enrichment and the
			// disk-path "daemon not running" warning.
			live, dialErr := control.DialState(dir)

			// TTY + live: enriched output (paused state, applied snapshot, sync age).
			// Piped/non-TTY callers skip this block — enrichment gated behind isTTY
			// so script-parseable fields stay stable.
			if isTTY(cmd) && dialErr == nil {
				fmt.Fprintf(out, "device:  %s\n", id.Fingerprint())
				fmt.Fprintf(out, "hub:     %s\n", orNone(d.Hub))
				paused := ""
				if live.Paused {
					paused = " ⏸️  PAUSED"
				}
				fmt.Fprintf(out, "mounts:  %d (live)%s\n", len(live.Mounts), paused)
				for _, m := range live.Mounts {
					mode := "rw"
					if m.ReadOnly {
						mode = "ro"
					}
					if m.Pinned {
						mode += ",pinned"
					}
					fmt.Fprintf(out, "  - %s/%s -> %s [%s] @%s  (%s)\n",
						m.Share, m.Subpath, m.Local, mode, short(orNone(m.BaseSnapshot)), syncAge(m.LastSyncUnix))
					if m.LastErr != "" {
						fmt.Fprintf(out, "      ⚠️  last sync failed: %s\n", m.LastErr)
					}
				}
				return nil
			}

			// Disk path (non-TTY or TTY with daemon not running).
			if dialErr != nil {
				fmt.Fprintln(out, "⚠️  daemon not running — showing last-known disk state (run: devbox start)")
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
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON (prefers live daemon state)")
	return cmd
}

// syncAge renders how long ago a mount last synced (for live status).
func syncAge(unix int64) string {
	if unix == 0 {
		return "not synced yet"
	}
	d := time.Since(time.Unix(unix, 0)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("synced %ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("synced %dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("synced %dh ago", int(d.Hours()))
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
				// A pidfile we couldn't validate as our live daemon (gone, or a
				// recycled PID now belonging to some other process) is stale —
				// drop it rather than risk SIGTERMing an unrelated process.
				if _, exists := os.Stat(pidPath(dir)); exists == nil {
					removePid(dir)
					return fmt.Errorf("no running daemon (removed stale pidfile)")
				}
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

func doctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "🩺 diagnose this device's devbox setup (config, hub, mounts, watcher)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			type doctorCheck struct {
				Name   string `json:"name"`
				Status string `json:"status"` // ok | warn | fail
				Detail string `json:"detail,omitempty"`
			}
			var checks []doctorCheck
			var failed bool
			check := func(ok bool, warn bool, name, detail string) {
				glyph, status := "✅", "ok"
				if !ok {
					if warn {
						glyph, status = "⚠️ ", "warn"
					} else {
						glyph, status, failed = "❌", "fail", true
					}
				}
				checks = append(checks, doctorCheck{Name: name, Status: status, Detail: detail})
				if asJSON {
					return
				}
				if detail != "" {
					fmt.Fprintf(out, "%s %s — %s\n", glyph, name, detail)
				} else {
					fmt.Fprintf(out, "%s %s\n", glyph, name)
				}
			}
			// finish emits the JSON report (when --json) and preserves the exit code.
			finish := func(retErr error) error {
				if asJSON {
					_ = writeJSON(out, checks)
				}
				return retErr
			}

			if !asJSON {
				fmt.Fprintf(out, "devbox %s · %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
				fmt.Fprintf(out, "config: %s\n\n", dir)
			}

			// Identity + join state.
			if _, err := identity.Load(dir); err != nil {
				check(false, false, "identity", "not joined — run: devbox join <hub> <token>")
				return finish(fmt.Errorf("device is not joined"))
			}
			check(true, false, "identity", "device keypair present")

			d, err := config.LoadDaemon(dir)
			if err != nil {
				check(false, false, "config", err.Error())
				return finish(err)
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

			// Clock-skew check (diagnostic-only — never a hard failure).
			if d.Hub != "" {
				if hubDate, err := transport.New(d.Hub).HubDate(); err == nil {
					skew := time.Since(hubDate)
					ok, warn := clockSkewStatus(skew)
					if ok {
						check(true, false, "clock skew", skewAbs(skew).Round(time.Millisecond).String()+" vs hub")
					} else {
						check(false, warn, "clock skew", "device clock is "+skewAbs(skew).Round(time.Second).String()+" off the hub — fix NTP; join/snapshot ordering may misbehave")
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

			// macOS Full Disk Access — needed to sync TCC-protected folders
			// (~/Desktop, ~/Documents, ~/Downloads, iCloud). A background daemon
			// can't show the per-folder prompt, so without FDA those mounts fail
			// silently. We test the actual protected mounts (honest, version-proof):
			// only the ones we'd really sync, and only flag the ones truly blocked.
			if prot := protectedMounts(d.Mounts); len(prot) > 0 {
				if blocked := blockedByTCC(prot); len(blocked) == 0 {
					check(true, false, "full disk access", "protected mounts readable ("+strings.Join(prot, ", ")+")")
				} else {
					exe, _ := os.Executable()
					check(false, false, "full disk access", "NOT granted — can't read "+strings.Join(blocked, ", ")+
						"\n     fix: System Settings → Privacy & Security → Full Disk Access → + add "+exe+
						"\n     open it: open \"x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles\"")
				}
			}

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

			if failed {
				if !asJSON {
					fmt.Fprintln(out)
				}
				return finish(fmt.Errorf("doctor found problems (see ❌ above)"))
			}
			if !asJSON {
				fmt.Fprintln(out, "\n🎉 all checks passed")
			}
			return finish(nil)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON (exit code still non-zero on failure)")
	return cmd
}

// clockSkewStatus returns ok=true when skew is within the 30-second tolerance,
// warn=true (never a hard failure) when it exceeds it.
func clockSkewStatus(skew time.Duration) (ok, warn bool) {
	if skewAbs(skew) <= 30*time.Second {
		return true, false
	}
	return false, true
}

// skewAbs returns the absolute value of a duration.
func skewAbs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
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
