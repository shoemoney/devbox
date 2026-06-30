//go:build unix

package main

import "syscall"

// freeBytes returns the number of bytes available to an unprivileged process
// on the filesystem containing path. Returns ok=false if the stat fails.
func freeBytes(path string) (uint64, bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, false
	}
	return s.Bavail * uint64(s.Bsize), true // ponytail: Bsize is int32 on macOS, int64 on Linux; always positive
}
