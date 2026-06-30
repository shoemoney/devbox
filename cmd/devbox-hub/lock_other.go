//go:build !unix

package main

// dataLock is a no-op on non-unix platforms (the hub ships for linux/darwin via
// Docker/systemd; flock isn't available on Windows/plan9). gc/backup overlap is
// only a concern on a live server, which is unix. ponytail: add winio LockFileEx
// if a Windows hub ever ships.
func dataLock(_ string) (func(), error) { return func() {}, nil }
