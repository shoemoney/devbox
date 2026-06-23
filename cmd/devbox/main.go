// Command devbox is the device-side CLI + daemon entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/identity"
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
		statusCmd(),
		versionCmd(),
		stub("start", "▶️  run the sync daemon", "M3"),
		stub("stop", "⏹️  stop the sync daemon", "M3"),
		stub("publish", "📂 create a share from a local folder", "M2"),
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
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			id, err := identity.LoadOrCreate(dir)
			if err != nil {
				return err
			}
			d, err := config.LoadDaemon(dir)
			if err != nil {
				return err
			}
			d.Hub = args[0]
			if err := config.SaveDaemon(dir, d); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "device fingerprint: %s\n", id.Fingerprint())
			fmt.Fprintf(out, "hub:                %s\n", args[0])
			fmt.Fprintln(out, "✅ identity ready. (hub handshake lands in M2)")
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
			d, _ := config.LoadDaemon(dir)
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

func stub(use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%q is not implemented yet (arrives in %s)", use, milestone)
		},
	}
}
