//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// dataLock takes an exclusive advisory lock on <data>/.lock so the gc sweep and
// `backup` can never run at the same time — otherwise gc could delete a blob
// mid-backup, leaving the backup's DB referencing a blob its copy missed. flock
// is released automatically if the process dies, so a crash can't wedge it.
// Returns a release func; call it (idempotent enough) when the critical section ends.
func dataLock(data string) (func(), error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(data, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
