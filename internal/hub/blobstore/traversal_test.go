package blobstore

import (
	"os"
	"path/filepath"
	"testing"
)

// A blob key that tries to escape the root (path traversal) must be rejected,
// not resolved to a file outside the store.
func TestDiskRejectsPathTraversalKeys(t *testing.T) {
	root := t.TempDir()
	// A secret living OUTSIDE the blob root that an attacker would target.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDisk(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"..", "..a", // first two chars ".." would escape via shardDir
		"a/b", `a\b`,
		".",
	} {
		if _, err := d.Get(key); err == nil {
			t.Errorf("Get(%q) returned no error — traversal not blocked", key)
		}
		if has, err := d.Has(key); err == nil && has {
			t.Errorf("Has(%q) = true — traversal not blocked", key)
		}
		if err := d.Delete(key); err == nil {
			t.Errorf("Delete(%q) returned no error — traversal not blocked", key)
		}
	}

	// The targeted secret must still be intact (never read or removed).
	if b, err := os.ReadFile(secret); err != nil || string(b) != "top-secret" {
		t.Fatalf("secret outside root was touched: %q, %v", b, err)
	}

	// A well-formed key still round-trips.
	good := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := d.Put(good, []byte("ok")); err != nil {
		t.Fatalf("Put(valid) = %v", err)
	}
	if b, err := d.Get(good); err != nil || string(b) != "ok" {
		t.Fatalf("Get(valid) = %q, %v", b, err)
	}
}
