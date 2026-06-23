// Command devbox-hub is the hub server + admin CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.0.0-dev"

func main() {
	root := &cobra.Command{
		Use:           "devbox-hub",
		Short:         "🛰️  devbox hub server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "print version",
			Run:   func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), "devbox-hub", version) },
		},
		hubStub("serve", "🚀 run the hub server", "M2"),
		hubStub("token", "🎟️  mint/rotate the join token", "M2"),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func hubStub(use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%q not implemented yet (arrives in %s)", use, milestone)
		},
	}
}
