//go:build !darwin

package main

import "git.shoemoney.ai/shoemoney/devbox/internal/config"

// Full Disk Access is a macOS concept; elsewhere these are no-ops. (Windows has a
// loose analog — Controlled Folder Access — but it's off by default; install.ps1
// notes how to allowlist devbox if a user has it on.)
func protectedMounts([]config.Mount) []string { return nil }
func blockedByTCC([]string) []string          { return nil }
