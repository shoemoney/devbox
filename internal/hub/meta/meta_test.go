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
