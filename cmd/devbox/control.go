package main

import (
	"errors"
	"fmt"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/control"
)

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "⏸️  pause syncing on the running daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			if err := control.Pause(dir); err != nil {
				return daemonHint(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "⏸️  paused — run: devbox resume to continue")
			return nil
		},
	}
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "▶️  resume syncing on the running daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			if err := control.Resume(dir); err != nil {
				return daemonHint(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "▶️  resumed — catching up all mounts")
			return nil
		},
	}
}

// daemonHint turns the "no control socket" error into a user-facing message,
// passing other errors through unchanged.
func daemonHint(err error) error {
	if errors.Is(err, control.ErrNotRunning) {
		return fmt.Errorf("daemon not running — run: devbox start")
	}
	return err
}

// isTTY reports whether the command's stdout is an interactive terminal. Used to
// decide whether status may show the human-only "(live)" enrichment — piped
// output stays byte-identical to v1 so scripts are unaffected.
func isTTY(cmd *cobra.Command) bool {
	f, ok := cmd.OutOrStdout().(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
