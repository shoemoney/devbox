package blobstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// compile-time check: Disk satisfies Store.
var _ Store = (*Disk)(nil)

func TestPutHasGetDelete(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const hash = "abcd1234"
	data := []byte("hello chunk")

	if ok, _ := d.Has(hash); ok {
		t.Fatal("blob should not exist yet")
	}
	if err := d.Put(hash, data); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Has(hash); err != nil || !ok {
		t.Fatalf("expected blob to exist (ok=%v err=%v)", ok, err)
	}
	got, err := d.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	// stored under 2-char shard
	if _, err := os.Stat(filepath.Join(d.root, "ab", hash)); err != nil {
		t.Fatalf("expected sharded path ab/%s: %v", hash, err)
	}

	if err := d.Delete(hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Has(hash); ok {
		t.Fatal("blob should be gone after delete")
	}
}

func TestPutIdempotent(t *testing.T) {
	d, _ := NewDisk(t.TempDir())
	if err := d.Put("ff00", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	// Put again with same key is a no-op (content-addressed: same key == same bytes).
	if err := d.Put("ff00", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Get("ff00")
	if string(got) != "v1" {
		t.Fatalf("got %q", got)
	}
}

func TestMissingErrors(t *testing.T) {
	d, _ := NewDisk(t.TempDir())
	if _, err := d.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}
	if err := d.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: want ErrNotFound, got %v", err)
	}
}

func TestNoTempLeftBehind(t *testing.T) {
	root := t.TempDir()
	d, _ := NewDisk(root)
	if err := d.Put("dead", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// the .tmp-* file must not survive a successful Put
	matches, _ := filepath.Glob(filepath.Join(root, "de", ".tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}
