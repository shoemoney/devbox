//go:build !unix

package main

// freeBytes is not implemented on non-unix platforms; callers skip the check silently.
func freeBytes(_ string) (uint64, bool) { return 0, false }
