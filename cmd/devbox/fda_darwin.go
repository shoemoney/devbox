//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shoemoney/devbox/internal/config"
)

// protectedMounts returns the local mount paths that live under a macOS TCC-
// protected directory (Desktop/Documents/Downloads/iCloud). A daemon can't sync
// these without Full Disk Access — and being a background agent, it can't even
// show the per-folder prompt, so it just silently fails until FDA is granted.
func protectedMounts(mounts []config.Mount) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	guarded := []string{
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Library", "Mobile Documents"), // iCloud Drive
	}
	var out []string
	for _, m := range mounts {
		abs, err := filepath.Abs(m.Local)
		if err != nil {
			continue
		}
		for _, g := range guarded {
			if abs == g || strings.HasPrefix(abs, g+string(os.PathSeparator)) {
				out = append(out, m.Local)
				break
			}
		}
	}
	return out
}

// blockedByTCC returns the protected paths this process currently CANNOT read
// (permission denied) — the honest signal that Full Disk Access is missing, since
// it tests the exact dirs we sync rather than a TCC.db probe that varies by macOS
// version. A path that reads fine, or doesn't exist yet, is not reported.
func blockedByTCC(paths []string) []string {
	var blocked []string
	for _, p := range paths {
		if _, err := os.ReadDir(p); err != nil && os.IsPermission(err) {
			blocked = append(blocked, p)
		}
	}
	return blocked
}
