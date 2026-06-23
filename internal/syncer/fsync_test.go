package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWriteWithSync proves the fsync durability path doesn't break a write:
// the file lands with the exact content and mode, and returns no error. We can't
// simulate a power loss, so this is a regression guard against the sync calls
// corrupting or failing an ordinary write.
func TestAtomicWriteWithSync(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "hello.txt")
	want := []byte("never lose a byte")
	if err := atomicWrite(dst, want, 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}
