package syncer

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
)

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// TestTwoWayConvergeAndConflict is the heart of devbox: two devices sharing one
// share, a concurrent edit to the same file, and full convergence with NO data
// loss — hub canonical wins, the loser becomes a conflict copy, and that copy
// propagates to every device.
func TestTwoWayConvergeAndConflict(t *testing.T) {
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

	A := joinDevice(t, db, srv.URL, "alice")
	B := joinDevice(t, db, srv.URL, "bob")
	if err := A.Publish("s"); err != nil {
		t.Fatal(err)
	}
	rootA, rootB := t.TempDir(), t.TempDir()

	// A creates foo + bar and syncs them up.
	writeFile(t, rootA, "foo.txt", "A1\n")
	writeFile(t, rootA, "bar.txt", "shared\n")
	baseA, _, err := Sync(A, rootA, "s", "", "alice", 1000, ig, guard)
	if err != nil {
		t.Fatalf("A sync: %v", err)
	}

	// B clones the share into an empty dir.
	baseB, _, err := Sync(B, rootB, "s", "", "bob", 1001, ig, guard)
	if err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if read(t, rootB, "foo.txt") != "A1\n" || read(t, rootB, "bar.txt") != "shared\n" {
		t.Fatal("B did not clone A's files")
	}

	// A edits foo.txt; it becomes the hub head.
	writeFile(t, rootA, "foo.txt", "A2\n")
	baseA, _, err = Sync(A, rootA, "s", baseA, "alice", 1002, ig, guard)
	if err != nil {
		t.Fatalf("A sync 2: %v", err)
	}

	// B edits the SAME file (still on the old base) and syncs -> conflict.
	writeFile(t, rootB, "foo.txt", "B2\n")
	_, prB2, err := Sync(B, rootB, "s", baseB, "bob", 1003, ig, guard)
	if err != nil {
		t.Fatalf("B sync 2: %v", err)
	}
	if read(t, rootB, "foo.txt") != "A2\n" {
		t.Fatalf("B foo.txt = %q, want canonical A2", read(t, rootB, "foo.txt"))
	}
	if len(prB2.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict copy on B, got %v", prB2.Conflicts)
	}
	if read(t, rootB, prB2.Conflicts[0]) != "B2\n" {
		t.Fatalf("conflict copy = %q, want B2 (no byte lost)", read(t, rootB, prB2.Conflicts[0]))
	}

	// A syncs again and must receive B's conflict copy — full convergence.
	if _, _, err = Sync(A, rootA, "s", baseA, "alice", 1004, ig, guard); err != nil {
		t.Fatalf("A sync 3: %v", err)
	}
	if read(t, rootA, "foo.txt") != "A2\n" {
		t.Fatal("A foo.txt should still be A2")
	}
	if !exists(rootA, prB2.Conflicts[0]) {
		t.Fatalf("A did not receive B's conflict copy %q", prB2.Conflicts[0])
	}
	if read(t, rootA, prB2.Conflicts[0]) != "B2\n" {
		t.Fatalf("A's copy of conflict = %q, want B2", read(t, rootA, prB2.Conflicts[0]))
	}
}

// TestPullDeletePropagates checks a hub-side delete reaches a clone, and a local
// edit beats a hub delete (the edit survives).
func TestPullDeleteAndEditBeatsDelete(t *testing.T) {
	db, _ := meta.Open(":memory:")
	defer db.Close()
	store, _ := blobstore.NewDisk(t.TempDir())
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()
	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	A := joinDevice(t, db, srv.URL, "alice")
	B := joinDevice(t, db, srv.URL, "bob")
	_ = A.Publish("s")
	rootA, rootB := t.TempDir(), t.TempDir()

	writeFile(t, rootA, "keep.txt", "k\n")
	writeFile(t, rootA, "gone.txt", "g\n")
	baseA, _, _ := Sync(A, rootA, "s", "", "alice", 1, ig, guard)
	baseB, _, _ := Sync(B, rootB, "s", "", "bob", 2, ig, guard)

	// A deletes gone.txt; B (clone) should drop it on next sync.
	os.Remove(filepath.Join(rootA, "gone.txt"))
	baseA, _, _ = Sync(A, rootA, "s", baseA, "alice", 3, ig, guard)
	if _, _, err := Sync(B, rootB, "s", baseB, "bob", 4, ig, guard); err != nil {
		t.Fatal(err)
	}
	if exists(rootB, "gone.txt") {
		t.Fatal("hub delete did not propagate to B")
	}
}

// TestPullSkipsFileDirClash: a hub file whose local path is a directory must be
// skipped (recorded), not abort the whole pull (C3 resilience).
func TestPullSkipsFileDirClash(t *testing.T) {
	db, _ := meta.Open(":memory:")
	defer db.Close()
	store, _ := blobstore.NewDisk(t.TempDir())
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()
	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	A := joinDevice(t, db, srv.URL, "alice")
	B := joinDevice(t, db, srv.URL, "bob")
	_ = A.Publish("s")

	rootA := t.TempDir()
	writeFile(t, rootA, "data", "hello\n")
	writeFile(t, rootA, "ok.txt", "fine\n")
	if _, _, err := Sync(A, rootA, "s", "", "alice", 1, ig, guard); err != nil {
		t.Fatal(err)
	}

	// Bob's mount has a DIRECTORY where the hub has a file "data".
	rootB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootB, "data", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	pr, err := Pull(B, rootB, "s", "", "bob", 2, ig, guard)
	if err != nil {
		t.Fatalf("pull must not abort on a file/dir clash: %v", err)
	}
	if len(pr.Skipped) != 1 || pr.Skipped[0] != "data" {
		t.Fatalf("Skipped = %v, want [data]", pr.Skipped)
	}
	// The non-clashing file still applied.
	if !exists(rootB, "ok.txt") {
		t.Fatal("ok.txt should have been written despite the data/ clash")
	}
}
