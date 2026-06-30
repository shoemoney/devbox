package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/hub"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/secret"
	"github.com/shoemoney/devbox/internal/syncer"
	"github.com/shoemoney/devbox/internal/transport"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGCDropsOldUniqueChunks pushes two snapshots, gc's with keep=1, and asserts
// the pruned snapshot's UNIQUE chunk blob is gone while a chunk still referenced
// by the surviving head remains — and the head itself is untouched.
func TestGCDropsOldUniqueChunks(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()

	// Enroll a device.
	if err := db.CreateToken(hub.HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	c := transport.New(srv.URL)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join("tok", "dev", pub, priv); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("proj"); err != nil {
		t.Fatal(err)
	}

	guard, _ := secret.New(nil)
	root := t.TempDir()
	ig, _ := syncer.LoadIgnore(root)

	// v1: shared.txt (kept across versions) + only-v1.txt (unique to snap1).
	writeFile(t, root, "shared.txt", "stable content\n")
	writeFile(t, root, "only-v1.txt", "this file vanishes in v2\n")
	v1, err := syncer.Push(c, root, "proj", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}

	// v2: shared.txt unchanged, only-v1.txt deleted, only-v2.txt added.
	if err := os.Remove(filepath.Join(root, "only-v1.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "only-v2.txt", "fresh in v2\n")
	v2, err := syncer.Push(c, root, "proj", "", ig, guard, v1.Head, nil)
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}
	if v2.Snapshot == v1.Snapshot {
		t.Fatal("v2 must differ from v1")
	}

	// Capture chunk hashes per snapshot before gc.
	v1Chunks := manifestChunksOrFail(t, store, v1.Snapshot)
	v2Chunks := manifestChunksOrFail(t, store, v2.Snapshot)
	uniqueV1 := diff(v1Chunks, v2Chunks) // chunks only v1 referenced
	shared := intersect(v1Chunks, v2Chunks)
	if len(uniqueV1) == 0 {
		t.Fatal("test setup: expected at least one v1-only chunk")
	}
	if len(shared) == 0 {
		t.Fatal("test setup: expected at least one shared chunk")
	}

	// gc keeping only the newest snapshot (v2 == head): v1 is pruned.
	snaps, chunks, err := runGC(db, store, 1, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if snaps != 1 {
		t.Fatalf("gc removed %d snapshots, want 1", snaps)
	}
	if chunks != len(uniqueV1) {
		t.Fatalf("gc freed %d chunks, want %d (v1-only)", chunks, len(uniqueV1))
	}

	// v1's unique chunk blobs are gone.
	for _, h := range uniqueV1 {
		if has, _ := store.Has(h); has {
			t.Fatalf("unique v1 chunk %s should have been deleted", h)
		}
		if rc, _ := db.ChunkRefcount(h); rc != 0 {
			t.Fatalf("unique v1 chunk %s refcount = %d, want 0/gone", h, rc)
		}
	}
	// Shared chunks survive (still referenced by the head).
	for _, h := range shared {
		if has, _ := store.Has(h); !has {
			t.Fatalf("shared chunk %s must survive gc", h)
		}
	}
	// v1's manifest blob is gone; v2's (the head's) survives.
	if has, _ := store.Has(v1.Snapshot); has {
		t.Fatal("pruned v1 manifest blob should be deleted")
	}
	if has, _ := store.Has(v2.Snapshot); !has {
		t.Fatal("head v2 manifest blob must survive")
	}
	// The head is intact.
	if head, _, _ := db.ShareHead("proj"); head != v2.Snapshot {
		t.Fatalf("head = %q after gc, want %q", head, v2.Snapshot)
	}
	// And v2's chunks are all still present.
	for _, h := range v2Chunks {
		if has, _ := store.Has(h); !has {
			t.Fatalf("head chunk %s must survive gc", h)
		}
	}
}

// TestGCDryRun asserts --dry-run reports the same counts the real sweep would
// remove while deleting nothing, then that the real run matches those counts.
func TestGCDryRun(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()

	if err := db.CreateToken(hub.HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	c := transport.New(srv.URL)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := c.Join("tok", "dev", pub, priv); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("proj"); err != nil {
		t.Fatal(err)
	}
	guard, _ := secret.New(nil)
	root := t.TempDir()
	ig, _ := syncer.LoadIgnore(root)

	writeFile(t, root, "shared.txt", "stable content\n")
	writeFile(t, root, "only-v1.txt", "this file vanishes in v2\n")
	v1, err := syncer.Push(c, root, "proj", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "only-v1.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "only-v2.txt", "fresh in v2\n")
	v2, err := syncer.Push(c, root, "proj", "", ig, guard, v1.Head, nil)
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}
	uniqueV1 := diff(manifestChunksOrFail(t, store, v1.Snapshot), manifestChunksOrFail(t, store, v2.Snapshot))
	if len(uniqueV1) == 0 {
		t.Fatal("test setup: expected at least one v1-only chunk")
	}

	// Dry-run reports what would go, deletes nothing.
	dSnaps, dChunks, err := runGC(db, store, 1, true)
	if err != nil {
		t.Fatalf("dry-run gc: %v", err)
	}
	if dSnaps != 1 || dChunks != len(uniqueV1) {
		t.Fatalf("dry-run reported %d snaps / %d chunks, want 1 / %d", dSnaps, dChunks, len(uniqueV1))
	}
	if has, _ := store.Has(v1.Snapshot); !has {
		t.Fatal("dry-run must NOT delete the v1 manifest blob")
	}
	for _, h := range uniqueV1 {
		if has, _ := store.Has(h); !has {
			t.Fatalf("dry-run must NOT delete chunk %s", h)
		}
	}

	// Real run matches the dry-run prediction.
	rSnaps, rChunks, err := runGC(db, store, 1, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if rSnaps != dSnaps || rChunks != dChunks {
		t.Fatalf("real gc removed %d snaps / %d chunks, dry-run predicted %d / %d", rSnaps, rChunks, dSnaps, dChunks)
	}
}

func manifestChunksOrFail(t *testing.T, store blobstore.Store, id string) []string {
	t.Helper()
	hs, err := manifestChunks(store, id)
	if err != nil {
		t.Fatalf("manifestChunks(%s): %v", id, err)
	}
	return hs
}

// diff returns elements of a not present in b.
func diff(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if !set[x] {
			out = append(out, x)
		}
	}
	return out
}

// intersect returns elements present in both a and b.
func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}
