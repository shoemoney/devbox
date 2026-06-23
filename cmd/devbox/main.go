// Command devbox is the device-side CLI + daemon entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
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
		statusCmd(),
		versionCmd(),
		stub("start", "▶️  run the sync daemon", "M3"),
		stub("stop", "⏹️  stop the sync daemon", "M3"),
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
			res, err := syncer.Push(c, root, share, ig, guard, head)
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
