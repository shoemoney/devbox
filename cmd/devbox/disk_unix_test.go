//go:build unix

package main

import "testing"

func TestFreeBytesBasic(t *testing.T) {
	free, ok := freeBytes(t.TempDir())
	if !ok {
		t.Fatal("freeBytes on TempDir must return ok=true on unix")
	}
	if free == 0 {
		t.Fatal("freeBytes returned 0; expected > 0 for a real filesystem")
	}
}
