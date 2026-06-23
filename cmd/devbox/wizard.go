package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
)

// setupSkippedFile, when present in the config dir, means the user chose "don't
// ask again" — the first-run wizard is never auto-offered after that.
const setupSkippedFile = "setup-skipped"

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "🧭 step-by-step first-time setup wizard",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			return runWizard(os.Stdin, cmd.OutOrStdout(), dir)
		},
	}
}

// maybeOfferSetup offers the wizard on a bare `devbox` when the machine isn't set
// up and the user hasn't opted out — and ONLY on a real terminal, so scripts and
// pipes are never blocked on a prompt. Returns true if it ran the wizard (so the
// caller skips the usage dump).
func maybeOfferSetup(dir string) bool {
	if !stdinIsTTY() || !shouldOfferSetup(dir) {
		return false
	}
	ran, _ := offerSetup(os.Stdin, os.Stdout, dir)
	return ran
}

// shouldOfferSetup is true only before the first join and before any opt-out.
func shouldOfferSetup(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, setupSkippedFile)); err == nil {
		return false // opted out
	}
	d, err := config.LoadDaemon(dir)
	return err == nil && d.Hub == "" // not joined yet
}

// offerSetup asks the one question. "n…" records the opt-out (never ask again);
// anything else launches the wizard. ranWizard reports whether the wizard ran.
func offerSetup(in io.Reader, out io.Writer, dir string) (ranWizard bool, err error) {
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "👋 devbox isn't set up on this machine yet.")
	fmt.Fprintln(out, "Would you like a step-by-step setup wizard?")
	fmt.Fprintln(out, "  [Y] yes please")
	fmt.Fprintln(out, "  [n] no, I'm a devbox expert — don't ask again")
	fmt.Fprint(out, "> ")
	line, _ := r.ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "n") {
		if err := markSetupSkipped(dir); err != nil {
			return false, err
		}
		fmt.Fprintln(out, "👍 You're flying solo — run `devbox setup` anytime to change your mind.")
		return false, nil
	}
	return true, runWizardReader(r, out, dir)
}

func markSetupSkipped(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, setupSkippedFile), []byte("skipped\n"), 0o644)
}

// runWizard walks a new user through the one genuinely fiddly step — enrolling
// against a hub — then points at the next commands. Mount/publish are left as
// printed next-steps: they're self-explanatory once joined (ponytail: the wizard
// owns the hard part, not a reimplementation of every command).
func runWizard(in io.Reader, out io.Writer, dir string) error {
	return runWizardReader(bufio.NewReader(in), out, dir)
}

func runWizardReader(r *bufio.Reader, out io.Writer, dir string) error {
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		s, _ := r.ReadString('\n')
		return strings.TrimSpace(s)
	}
	fmt.Fprintln(out, "\n🧭 Let's get this machine syncing.")
	hub := ask("\n1) Your hub URL (e.g. http://nas.local:8088):\n> ")
	if hub == "" {
		return fmt.Errorf("setup cancelled (no hub given)")
	}
	fmt.Fprintln(out, "\n2) A join token — get one on the hub with:  devbox-hub token")
	token := ask("   Paste it here:\n> ")
	if token == "" {
		return fmt.Errorf("setup cancelled (no token given)")
	}
	resp, err := joinHub(dir, hub, token)
	if err != nil {
		return fmt.Errorf("join failed: %w", err)
	}
	fmt.Fprintf(out, "\n✅ Joined as device %s.\n\nNext steps:\n", resp.DeviceID)
	fmt.Fprintln(out, "  • sync a hub folder here:   devbox mount <share> <dir>")
	fmt.Fprintln(out, "  • share a local folder:     devbox publish <dir> <share>")
	fmt.Fprintln(out, "  • then start syncing:       devbox start")
	return nil
}

// stdinIsTTY reports whether stdin is an interactive terminal (so we never prompt
// in a pipe or script). ponytail: stdlib ModeCharDevice, no go-isatty dep.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
