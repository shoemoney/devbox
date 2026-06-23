package meta

import "testing"

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDeviceLifecycle(t *testing.T) {
	db := open(t)
	if rev, _ := db.Revoked("dev1"); !rev {
		t.Fatal("unknown device should be treated as revoked")
	}
	if err := db.AddDevice("dev1", "laptop", []byte("pub"), 100); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revoked("dev1"); rev {
		t.Fatal("registered device should not be revoked")
	}
	if err := db.Revoke("dev1"); err != nil {
		t.Fatal(err)
	}
	if rev, _ := db.Revoked("dev1"); !rev {
		t.Fatal("revoked device should be revoked")
	}
}

func TestTokenRedeemOnce(t *testing.T) {
	db := open(t)
	if err := db.CreateToken("tok", 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.RedeemToken("tok", 500); !ok {
		t.Fatal("valid unexpired token should redeem")
	}
	if ok, _ := db.RedeemToken("tok", 500); ok {
		t.Fatal("token must not redeem twice")
	}

	if err := db.CreateToken("old", 100); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.RedeemToken("old", 200); ok {
		t.Fatal("expired token must not redeem")
	}
	if ok, _ := db.RedeemToken("ghost", 1); ok {
		t.Fatal("unknown token must not redeem")
	}
}

func TestWritableDefaultAndOverride(t *testing.T) {
	db := open(t)
	if w, _ := db.Writable("dev1", "projects"); !w {
		t.Fatal("default writable should be true")
	}
	if err := db.SetWritable("dev1", "projects", false); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.Writable("dev1", "projects"); w {
		t.Fatal("should be read-only after SetWritable(false)")
	}
	if err := db.SetWritable("dev1", "projects", true); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.Writable("dev1", "projects"); !w {
		t.Fatal("should be writable again after SetWritable(true)")
	}
}

func TestSharesSnapshotsRefcounts(t *testing.T) {
	db := open(t)
	if err := db.CreateShare("projects", "dev1", 1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.ShareHead("projects"); ok {
		t.Fatal("a new share should have no head yet")
	}

	// snap1 references h1 twice (must count once) and h2.
	s1 := Snapshot{ID: "snap1", Share: "projects", DeviceID: "dev1", ManifestHash: "m1", CreatedAt: 10}
	if err := db.AddSnapshot(s1, []ChunkRef{{"h1", 5}, {"h2", 6}, {"h1", 5}}); err != nil {
		t.Fatal(err)
	}
	head, ok, _ := db.ShareHead("projects")
	if !ok || head != "snap1" {
		t.Fatalf("head = %q ok=%v, want snap1", head, ok)
	}
	if rc, _ := db.ChunkRefcount("h1"); rc != 1 {
		t.Fatalf("h1 refcount = %d, want 1 (deduped within a snapshot)", rc)
	}

	// snap2 re-uses h1 and adds h3.
	s2 := Snapshot{ID: "snap2", Share: "projects", ParentID: "snap1", DeviceID: "dev1", ManifestHash: "m2", CreatedAt: 20}
	if err := db.AddSnapshot(s2, []ChunkRef{{"h1", 5}, {"h3", 7}}); err != nil {
		t.Fatal(err)
	}
	if rc, _ := db.ChunkRefcount("h1"); rc != 2 {
		t.Fatalf("h1 refcount = %d, want 2 (two snapshots reference it)", rc)
	}
	if rc, _ := db.ChunkRefcount("h3"); rc != 1 {
		t.Fatalf("h3 refcount = %d, want 1", rc)
	}
	if head, _, _ := db.ShareHead("projects"); head != "snap2" {
		t.Fatalf("head = %q, want snap2", head)
	}

	// Re-adding an existing snapshot is idempotent: no error, no double-count,
	// but it advances head (a revert to snap1's content).
	if err := db.AddSnapshot(s1, []ChunkRef{{"h1", 5}, {"h2", 6}}); err != nil {
		t.Fatalf("idempotent re-add failed: %v", err)
	}
	if rc, _ := db.ChunkRefcount("h1"); rc != 2 {
		t.Fatalf("h1 refcount = %d after re-add, want 2 (no double-count)", rc)
	}
	if head, _, _ := db.ShareHead("projects"); head != "snap1" {
		t.Fatalf("head = %q after re-add, want snap1 (revert)", head)
	}
}

// seedSnaps creates a share and a chain of snapshots with ascending timestamps.
func seedSnaps(t *testing.T, db *DB) {
	t.Helper()
	if err := db.CreateShare("proj", "dev1", 1); err != nil {
		t.Fatal(err)
	}
	snaps := []struct {
		id, parent string
		at         int64
		chunks     []ChunkRef
	}{
		{"s1", "", 10, []ChunkRef{{"h1", 5}, {"h2", 6}}},   // h1, h2
		{"s2", "s1", 20, []ChunkRef{{"h1", 5}, {"h3", 7}}}, // shares h1, adds h3
		{"s3", "s2", 30, []ChunkRef{{"h1", 5}, {"h4", 8}}}, // shares h1, adds h4 (head)
	}
	for _, s := range snaps {
		if err := db.AddSnapshot(Snapshot{ID: s.id, Share: "proj", ParentID: s.parent, DeviceID: "dev1", ManifestHash: s.id, CreatedAt: s.at}, s.chunks); err != nil {
			t.Fatalf("AddSnapshot %s: %v", s.id, err)
		}
	}
}

func TestSnapshotLogNewestFirst(t *testing.T) {
	db := open(t)
	seedSnaps(t, db)

	log, err := db.SnapshotLog("proj", 100)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"s3", "s2", "s1"}
	if len(log) != len(wantIDs) {
		t.Fatalf("log len = %d, want %d", len(log), len(wantIDs))
	}
	for i, want := range wantIDs {
		if log[i].ID != want {
			t.Fatalf("log[%d].ID = %q, want %q (newest-first)", i, log[i].ID, want)
		}
	}
	if log[0].Parent != "s2" || log[2].Parent != "" {
		t.Fatalf("parents wrong: %q, %q", log[0].Parent, log[2].Parent)
	}
	if log[0].Device != "dev1" || log[0].CreatedAt != 30 {
		t.Fatalf("head meta = %+v", log[0])
	}

	// limit caps the result.
	if got, _ := db.SnapshotLog("proj", 2); len(got) != 2 || got[0].ID != "s3" {
		t.Fatalf("limited log = %+v, want first 2 newest", got)
	}
}

func TestDeletableSnapshotsKeepsHeadAndNewest(t *testing.T) {
	db := open(t)
	seedSnaps(t, db) // head = s3

	// keep=1 protects the newest (s3) which is also the head; s1, s2 are deletable.
	del, err := db.DeletableSnapshots("proj", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(del) != 2 || del[0] != "s2" || del[1] != "s1" {
		t.Fatalf("deletable(keep=1) = %v, want [s2 s1]", del)
	}

	// keep=2 protects s3 + s2; only s1 deletable.
	if del, _ := db.DeletableSnapshots("proj", 2); len(del) != 1 || del[0] != "s1" {
		t.Fatalf("deletable(keep=2) = %v, want [s1]", del)
	}

	// keep >= count: nothing deletable.
	if del, _ := db.DeletableSnapshots("proj", 10); len(del) != 0 {
		t.Fatalf("deletable(keep=10) = %v, want []", del)
	}

	// Even keep=0 must never delete the head.
	del0, _ := db.DeletableSnapshots("proj", 0)
	for _, id := range del0 {
		if id == "s3" {
			t.Fatal("keep=0 must still protect the head (s3)")
		}
	}
	if len(del0) != 2 {
		t.Fatalf("deletable(keep=0) = %v, want s1+s2 (head s3 protected)", del0)
	}
}

func TestDeleteSnapshotDecrementsRefcounts(t *testing.T) {
	db := open(t)
	seedSnaps(t, db)

	// Before: h1 referenced by s1,s2,s3 (rc=3); h2 only by s1 (rc=1).
	if rc, _ := db.ChunkRefcount("h1"); rc != 3 {
		t.Fatalf("h1 rc = %d, want 3", rc)
	}

	// Delete s1, which referenced h1 and h2.
	if err := db.DeleteSnapshot("s1", []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if rc, _ := db.ChunkRefcount("h1"); rc != 2 {
		t.Fatalf("h1 rc after delete = %d, want 2", rc)
	}
	if rc, _ := db.ChunkRefcount("h2"); rc != 0 {
		t.Fatalf("h2 rc after delete = %d, want 0", rc)
	}
	// The snapshot row is gone.
	if log, _ := db.SnapshotLog("proj", 100); len(log) != 2 {
		t.Fatalf("after delete log len = %d, want 2", len(log))
	}
}

func TestUnreferencedChunks(t *testing.T) {
	db := open(t)
	seedSnaps(t, db)

	if got, _ := db.UnreferencedChunks(); len(got) != 0 {
		t.Fatalf("nothing should be unreferenced yet, got %v", got)
	}

	// Drop s1: h2 falls to 0 and becomes collectable; h1 still has s2,s3.
	if err := db.DeleteSnapshot("s1", []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	un, err := db.UnreferencedChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0] != "h2" {
		t.Fatalf("unreferenced = %v, want [h2]", un)
	}

	if err := db.DeleteChunkRow("h2"); err != nil {
		t.Fatal(err)
	}
	if rc, _ := db.ChunkRefcount("h2"); rc != 0 {
		t.Fatalf("h2 should be gone, refcount lookup = %d", rc)
	}
	if got, _ := db.UnreferencedChunks(); len(got) != 0 {
		t.Fatalf("after row delete, unreferenced = %v, want []", got)
	}
}
