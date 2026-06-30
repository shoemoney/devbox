//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestDataLockMutualExclusion proves dataLock actually excludes a concurrent
// holder — the property gc/backup serialization depends on.
func TestDataLockMutualExclusion(t *testing.T) {
	data := t.TempDir()
	release, err := dataLock(data)
	if err != nil {
		t.Fatalf("dataLock: %v", err)
	}

	// A second exclusive, non-blocking flock must FAIL while the lock is held.
	f, err := os.OpenFile(filepath.Join(data, ".lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Fatal("second flock acquired while the lock was held — no mutual exclusion")
	}

	release()

	// After release, the lock is acquirable again.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock should succeed after release: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
