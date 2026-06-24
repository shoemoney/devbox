//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shoemoney/devbox/internal/config"
)

func TestProtectedMounts(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	mounts := []config.Mount{
		{Local: filepath.Join(home, "Documents", "work")},
		{Local: filepath.Join(home, "Desktop")},
		{Local: "/tmp/devbox-not-protected"},
		{Local: filepath.Join(home, "Library", "Mobile Documents", "iCloud~x")},
	}
	got := protectedMounts(mounts)
	if len(got) != 3 {
		t.Fatalf("protectedMounts = %v, want 3 (Documents, Desktop, iCloud — not /tmp)", got)
	}
	// A readable dir is never reported as TCC-blocked.
	if b := blockedByTCC([]string{t.TempDir()}); len(b) != 0 {
		t.Fatalf("a readable dir was reported blocked: %v", b)
	}
}
