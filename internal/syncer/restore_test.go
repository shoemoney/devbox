package syncer

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoemoney/devbox/internal/hub"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/secret"
)

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRestoreWholeTree pushes v1, edits to v2, then restores v1: the file content
// reverts and a fresh head snapshot is created (a restore is a new commit).
func TestRestoreWholeTree(t *testing.T) {
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

	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	c := joinDevice(t, db, srv.URL, "alice")
	if err := c.Publish("proj"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	// v1: two files.
	writeFile(t, root, "main.go", "v1\n")
	writeFile(t, root, "keep.txt", "constant\n")
	snap1, err := Push(c, root, "proj", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}

	// v2: edit main.go, add extra.txt.
	writeFile(t, root, "main.go", "v2 changed\n")
	writeFile(t, root, "extra.txt", "added later\n")
	snap2, err := Push(c, root, "proj", "", ig, guard, snap1.Head, nil)
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}
	if snap2.Snapshot == snap1.Snapshot {
		t.Fatal("v2 should be a different snapshot than v1")
	}

	// Restore v1.
	res, err := Restore(c, root, "proj", "", snap1.Snapshot, "", "host", 1, ig, guard, nil)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	// main.go reverts; keep.txt unchanged; extra.txt (not in v1) is deleted.
	if got := readFile(t, root, "main.go"); got != "v1\n" {
		t.Fatalf("main.go after restore = %q, want v1", got)
	}
	if got := readFile(t, root, "keep.txt"); got != "constant\n" {
		t.Fatalf("keep.txt = %q, want constant", got)
	}
	if _, err := os.Stat(filepath.Join(root, "extra.txt")); !os.IsNotExist(err) {
		t.Fatal("extra.txt should be deleted by a whole-tree restore to v1")
	}

	// The restore created a NEW head whose content equals v1 (content-addressed
	// snapshot id matches the original v1 snapshot).
	if res.Snapshot != snap1.Snapshot {
		t.Fatalf("restored head = %q, want v1 content id %q", res.Snapshot, snap1.Snapshot)
	}
	head, err := c.Head("proj")
	if err != nil {
		t.Fatal(err)
	}
	if head != snap1.Snapshot {
		t.Fatalf("hub head after restore = %q, want %q", head, snap1.Snapshot)
	}
}

// TestRestorePreservesUncommittedEdit is the never-lose-a-byte guarantee on a
// deliberate revert: an UNCOMMITTED local edit (differs from both the snapshot
// being restored and the hub head) survives as a .conflict copy, while the file
// itself reverts. A clean restore (TestRestoreWholeTree) preserves nothing.
func TestRestorePreservesUncommittedEdit(t *testing.T) {
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

	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())
	c := joinDevice(t, db, srv.URL, "alice")
	if err := c.Publish("proj"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	writeFile(t, root, "main.go", "v1\n")
	snap1, err := Push(c, root, "proj", "", ig, guard, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "main.go", "v2\n")
	if _, err := Push(c, root, "proj", "", ig, guard, snap1.Head, nil); err != nil {
		t.Fatal(err)
	}

	// Uncommitted local edit: v3 on disk, never pushed (head is still v2).
	writeFile(t, root, "main.go", "v3 UNCOMMITTED\n")

	if _, err := Restore(c, root, "proj", "", snap1.Snapshot, "", "host", 1, ig, guard, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The file reverted to v1...
	if got := readFile(t, root, "main.go"); got != "v1\n" {
		t.Fatalf("main.go = %q, want v1", got)
	}
	// ...and the uncommitted v3 survives as a conflict copy.
	matches, _ := filepath.Glob(filepath.Join(root, "main.conflict-*"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one conflict copy, got %v", matches)
	}
	if got, _ := os.ReadFile(matches[0]); string(got) != "v3 UNCOMMITTED\n" {
		t.Fatalf("conflict copy = %q, want the uncommitted v3", got)
	}
}

// TestRestoreSinglePath brings back just one file to its snapshot content without
// touching the rest of the tree, and errors cleanly on a path not in the snapshot.
func TestRestoreSinglePath(t *testing.T) {
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

	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	c := joinDevice(t, db, srv.URL, "bob")
	if err := c.Publish("proj"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	writeFile(t, root, "a.txt", "a-v1\n")
	writeFile(t, root, "b.txt", "b-v1\n")
	snap1, err := Push(c, root, "proj", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}

	// Edit both files.
	writeFile(t, root, "a.txt", "a-v2\n")
	writeFile(t, root, "b.txt", "b-v2\n")
	if _, err := Push(c, root, "proj", "", ig, guard, snap1.Head, nil); err != nil {
		t.Fatalf("push v2: %v", err)
	}

	// Restore only a.txt to v1; b.txt must keep its v2 content.
	if _, err := Restore(c, root, "proj", "", snap1.Snapshot, "a.txt", "host", 1, ig, guard, nil); err != nil {
		t.Fatalf("restore a.txt: %v", err)
	}
	if got := readFile(t, root, "a.txt"); got != "a-v1\n" {
		t.Fatalf("a.txt = %q, want a-v1 (restored)", got)
	}
	if got := readFile(t, root, "b.txt"); got != "b-v2\n" {
		t.Fatalf("b.txt = %q, want b-v2 (untouched)", got)
	}

	// A path not in the snapshot is rejected without writing anything.
	if _, err := Restore(c, root, "proj", "", snap1.Snapshot, "ghost.txt", "host", 1, ig, guard, nil); err == nil {
		t.Fatal("restoring a path absent from the snapshot should error")
	}
}
