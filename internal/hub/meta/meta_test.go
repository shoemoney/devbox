package meta

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	if err := db.DeleteSnapshot("proj", "s1", []string{"h1", "h2"}); err != nil {
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
	if err := db.DeleteSnapshot("proj", "s1", []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	un, err := db.UnreferencedChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0] != "h2" {
		t.Fatalf("unreferenced = %v, want [h2]", un)
	}

	if deleted, err := db.DeleteChunkRow("h2"); err != nil || !deleted {
		t.Fatalf("DeleteChunkRow(h2) = %v, %v; want true, nil", deleted, err)
	}
	if rc, _ := db.ChunkRefcount("h2"); rc != 0 {
		t.Fatalf("h2 should be gone, refcount lookup = %d", rc)
	}
	if got, _ := db.UnreferencedChunks(); len(got) != 0 {
		t.Fatalf("after row delete, unreferenced = %v, want []", got)
	}
}

// TestCrossShareRefcount is the v2 (M8) data-model fix: identical content pushed
// to two shares must count its chunks once PER SHARE. Under v1's global snapshot
// PK the second push hit the idempotent branch and never bumped the refcount
// (undercount) and recorded no row for the second share. With per-(share, id)
// snapshots each share accounts independently.
func TestCrossShareRefcount(t *testing.T) {
	db := open(t)
	for _, s := range []string{"a", "b"} {
		if err := db.CreateShare(s, "dev1", 1); err != nil {
			t.Fatal(err)
		}
	}
	// Same content id "shared" referencing chunk c1, pushed to both shares.
	snap := func(share string) Snapshot {
		return Snapshot{ID: "shared", Share: share, DeviceID: "dev1", ManifestHash: "shared", CreatedAt: 10}
	}
	if err := db.AddSnapshot(snap("a"), []ChunkRef{{"c1", 5}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSnapshot(snap("b"), []ChunkRef{{"c1", 5}}); err != nil {
		t.Fatal(err)
	}
	if rc, _ := db.ChunkRefcount("c1"); rc != 2 {
		t.Fatalf("c1 refcount = %d, want 2 (each share counts independently)", rc)
	}
	// Each share sees the snapshot in its own log + ids.
	for _, s := range []string{"a", "b"} {
		if log, _ := db.SnapshotLog(s, 10); len(log) != 1 || log[0].ID != "shared" {
			t.Fatalf("share %s log = %+v, want one 'shared' row", s, log)
		}
		if ids, _ := db.SnapshotIDs(s); len(ids) != 1 {
			t.Fatalf("share %s ids = %v, want [shared]", s, ids)
		}
	}
	// Pruning one share's copy leaves the other's refcount intact (rc 2 -> 1).
	if err := db.DeleteSnapshot("a", "shared", []string{"c1"}); err != nil {
		t.Fatal(err)
	}
	if rc, _ := db.ChunkRefcount("c1"); rc != 1 {
		t.Fatalf("c1 refcount after pruning share a = %d, want 1 (share b still references it)", rc)
	}
	if ids, _ := db.SnapshotIDs("b"); len(ids) != 1 {
		t.Fatalf("share b still has its snapshot after share a pruned, ids = %v", ids)
	}
}

// TestMigrationRunner checks the PRAGMA user_version runner: a fresh DB lands at
// the latest version, reopening is idempotent, and a DB written by a newer build
// is refused. Uses an on-disk DB so version + the pre-migration backup persist.
func TestMigrationRunner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if v := userVersion(t, db.sql); v != len(migrations) {
		t.Fatalf("fresh DB user_version = %d, want %d", v, len(migrations))
	}
	// Exercise the v2 schema end-to-end (composite PK) to prove the migration took.
	if err := db.CreateShare("s", "dev1", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSnapshot(Snapshot{ID: "x", Share: "s", DeviceID: "dev1", ManifestHash: "x", CreatedAt: 1}, []ChunkRef{{"c", 1}}); err != nil {
		t.Fatalf("AddSnapshot on migrated schema: %v", err)
	}
	db.Close()

	// Reopen: no pending migrations, must be a clean no-op.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if v := userVersion(t, db2.sql); v != len(migrations) {
		t.Fatalf("reopened DB user_version = %d, want %d", v, len(migrations))
	}
	if ids, _ := db2.SnapshotIDs("s"); len(ids) != 1 {
		t.Fatalf("data lost across reopen: ids = %v", ids)
	}
	db2.Close()

	// A DB from a newer devbox (version beyond what we know) must be refused, not
	// silently downgraded or corrupted.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("Open must refuse a database newer than this build")
	}
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRolesLegacyAndExplicit covers M8a write-side authorization: a fresh share
// is legacy (every known device is an implicit owner = v1), and the first member
// grant flips it to explicit/deny-by-default. Device->principal->role resolves.
func TestRolesLegacyAndExplicit(t *testing.T) {
	db := open(t)
	if err := db.AddDevice("devA", "laptop", []byte("k"), 1); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDevice("devB", "phone", []byte("k"), 1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("proj", "devA", 1); err != nil {
		t.Fatal(err)
	}

	// Legacy: both devices are implicit owners (v1 behavior preserved).
	for _, dev := range []string{"devA", "devB"} {
		role, _, explicit, err := db.EffectiveMember(dev, "proj")
		if err != nil {
			t.Fatal(err)
		}
		if explicit || role != RoleOwner {
			t.Fatalf("legacy %s: role=%d explicit=%v, want owner/false", dev, role, explicit)
		}
	}
	// An unknown device gets nothing, even in legacy mode.
	if role, _, _, _ := db.EffectiveMember("ghost", "proj"); role != 0 {
		t.Fatalf("unknown device role = %d, want 0", role)
	}

	// Put devB under its own principal, then grant that principal editor.
	if err := db.EnsurePrincipal("bob", "bob", 2); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDevicePrincipal("devB", "bob"); err != nil {
		t.Fatal(err)
	}
	// Grant the default 'owner' principal (devA) admin so devA keeps write after
	// the flip; grant bob editor.
	if err := db.SetMember("proj", "owner", RoleOwner, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMember("proj", "bob", RoleEditor, false); err != nil {
		t.Fatal(err)
	}

	// Now explicit: devA(owner) and devB(editor) both >= editor.
	for _, tc := range []struct {
		dev  string
		want int
	}{{"devA", RoleOwner}, {"devB", RoleEditor}} {
		role, _, explicit, err := db.EffectiveMember(tc.dev, "proj")
		if err != nil {
			t.Fatal(err)
		}
		if !explicit || role != tc.want {
			t.Fatalf("explicit %s: role=%d explicit=%v, want %d/true", tc.dev, role, explicit, tc.want)
		}
	}

	// Demote bob to viewer: below editor -> no write.
	if err := db.SetMember("proj", "bob", RoleViewer, false); err != nil {
		t.Fatal(err)
	}
	if role, _, _, _ := db.EffectiveMember("devB", "proj"); role >= RoleEditor {
		t.Fatalf("demoted devB role = %d, want < editor(%d)", role, RoleEditor)
	}

	// Remove bob entirely: deny-by-default (role 0) in explicit mode.
	if err := db.RemoveMember("proj", "bob"); err != nil {
		t.Fatal(err)
	}
	if role, _, explicit, _ := db.EffectiveMember("devB", "proj"); role != 0 || !explicit {
		t.Fatalf("removed devB role=%d explicit=%v, want 0/true (deny-by-default)", role, explicit)
	}

	ms, _ := db.Members("proj")
	if len(ms) != 1 || ms[0].Principal != "owner" {
		t.Fatalf("members = %+v, want just owner", ms)
	}
}

// TestMayGrant locks down the invite attenuation rules (the security core): a
// caller can never grant above their own role, never touch a superior, viewers
// can't grant, and editors need the +s reshare bit.
func TestMayGrant(t *testing.T) {
	cases := []struct {
		name                 string
		callerRole           int
		reshare              bool
		targetCurrent, grant int
		grantReshare         bool
		want                 bool
	}{
		{"owner grants editor", RoleOwner, false, 0, RoleEditor, false, true},
		{"owner grants owner (co-owner)", RoleOwner, false, 0, RoleOwner, false, true},
		{"admin grants admin", RoleAdmin, false, 0, RoleAdmin, false, true},
		{"admin can't grant owner (above self)", RoleAdmin, false, 0, RoleOwner, false, false},
		{"editor with +s grants editor", RoleEditor, true, 0, RoleEditor, false, true},
		{"editor with +s can't grant admin", RoleEditor, true, 0, RoleAdmin, false, false},
		{"editor WITHOUT +s can't grant", RoleEditor, false, 0, RoleViewer, false, false},
		{"viewer can't grant", RoleViewer, true, 0, RoleViewer, false, false},
		{"can't demote a superior owner", RoleEditor, true, RoleOwner, RoleViewer, false, false},
		{"invalid role rejected", RoleOwner, false, 0, 999, false, false},
		{"zero role (unmembered) can't grant", 0, false, 0, RoleViewer, false, false},
		// +s (reshare) attenuation: you can only confer the delegation bit you hold.
		{"editor+s confers +s", RoleEditor, true, 0, RoleEditor, true, true},
		{"admin WITHOUT +s can't confer +s", RoleAdmin, false, 0, RoleEditor, true, false},
		{"admin WITH +s confers +s", RoleAdmin, true, 0, RoleEditor, true, true},
		{"owner confers +s unconstrained", RoleOwner, false, 0, RoleEditor, true, true},
		{"editor+s confers +s but role still bounds (no admin)", RoleEditor, true, 0, RoleAdmin, true, false},
	}
	for _, c := range cases {
		if got := MayGrant(c.callerRole, c.reshare, c.targetCurrent, c.grant, c.grantReshare); got != c.want {
			t.Errorf("%s: MayGrant(%d,%v,%d,%d,%v)=%v, want %v", c.name, c.callerRole, c.reshare, c.targetCurrent, c.grant, c.grantReshare, got, c.want)
		}
	}
}

// TestInviteBinding round-trips an invite and confirms a plain token has none.
func TestInviteBinding(t *testing.T) {
	db := open(t)
	if err := db.CreateInvite("h1", 1000, Invite{Principal: "bob", Share: "proj", Role: RoleEditor, CanReshare: true}); err != nil {
		t.Fatal(err)
	}
	inv, ok, err := db.InviteBinding("h1")
	if err != nil || !ok {
		t.Fatalf("InviteBinding = %v, %v", ok, err)
	}
	if inv.Principal != "bob" || inv.Share != "proj" || inv.Role != RoleEditor || !inv.CanReshare {
		t.Fatalf("binding = %+v", inv)
	}
	// The invite token is redeemable like any join token.
	if redeemed, _ := db.RedeemToken("h1", 500); !redeemed {
		t.Fatal("invite token should redeem via the normal token path")
	}
	// A plain token has no binding.
	if err := db.CreateToken("plain", 1000); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.InviteBinding("plain"); ok {
		t.Fatal("a plain join token must have no invite binding")
	}
}

// TestMigrationFromV1WithData is the realistic upgrade path: a populated v1
// database (user_version 0, global snapshot PK) is migrated in place. Data must
// survive, a backup must be written, and the v1 idempotent-push legacy — a share
// whose head has no snapshot row of its own — must be repaired so every head has a
// row under the new per-(share, id) model.
func TestMigrationFromV1WithData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	// Hand-build a v1 database: baseline tables, user_version left at 0.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	// Share s owns content snapX (has its row). Share t's head is the SAME content
	// (the v1 idempotent push advanced t's head but recorded no (t, snapX) row).
	stmts := []string{
		`INSERT INTO shares(name, head_snapshot, created_by, created_at) VALUES('s','snapX','dev1',1)`,
		`INSERT INTO shares(name, head_snapshot, created_by, created_at) VALUES('t','snapX','dev1',1)`,
		`INSERT INTO snapshots(id,share,parent_id,device_id,created_at,manifest_hash) VALUES('snapX','s',NULL,'dev1',1,'snapX')`,
		`INSERT INTO chunks(hash,size,refcount) VALUES('c1',5,1)`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed v1 data %q: %v", s, err)
		}
	}
	raw.Close()

	db, err := Open(path) // triggers the v1 -> v2 migration
	if err != nil {
		t.Fatalf("migrating a populated v1 DB: %v", err)
	}
	defer db.Close()

	if v := userVersion(t, db.sql); v != len(migrations) {
		t.Fatalf("post-migration user_version = %d, want %d", v, len(migrations))
	}
	if _, err := os.Stat(path + ".pre-v2.bak"); err != nil {
		t.Fatalf("pre-migration backup missing: %v", err)
	}
	// Share s keeps its row.
	if ids, _ := db.SnapshotIDs("s"); len(ids) != 1 || ids[0] != "snapX" {
		t.Fatalf("share s ids = %v, want [snapX]", ids)
	}
	// Share t's head row was backfilled (the v1 legacy is repaired).
	if ids, _ := db.SnapshotIDs("t"); len(ids) != 1 || ids[0] != "snapX" {
		t.Fatalf("share t ids = %v, want [snapX] (head backfill)", ids)
	}
	if head, ok, _ := db.ShareHead("t"); !ok || head != "snapX" {
		t.Fatalf("share t head = %q ok=%v, want snapX", head, ok)
	}
	// The composite-PK schema is live: a new push to t records its own row.
	if err := db.AddSnapshot(Snapshot{ID: "snapY", Share: "t", ParentID: "snapX", DeviceID: "dev1", ManifestHash: "snapY", CreatedAt: 2}, []ChunkRef{{"c2", 6}}); err != nil {
		t.Fatalf("post-migration AddSnapshot: %v", err)
	}
	if ids, _ := db.SnapshotIDs("t"); len(ids) != 2 {
		t.Fatalf("share t ids after new push = %v, want 2", ids)
	}
}

// TestBackupTo verifies that BackupTo produces a readable copy of the database
// that contains the same rows as the live DB.
func TestBackupTo(t *testing.T) {
	dir := t.TempDir()
	// Use a file-backed DB — VACUUM INTO doesn't work on :memory:.
	db, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AddDevice("d1", "laptop", []byte("pub"), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backup.db")
	if err := db.BackupTo(dest); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	bak, err := Open(dest)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bak.Close()
	n, err := bak.Count("devices")
	if err != nil {
		t.Fatalf("backup Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("backup has %d devices, want 1", n)
	}
}

// TestDeviceLsShowsDevice verifies that Devices() surfaces a joined device.
func TestDeviceLsShowsDevice(t *testing.T) {
	db := open(t)
	now := time.Now().Unix()
	if err := db.AddDevice("d1", "laptop", []byte("pub"), now); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Devices returned %d rows, want 1", len(rows))
	}
	if rows[0].Name != "laptop" || rows[0].ID != "d1" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if rows[0].Revoked {
		t.Fatal("fresh device should not be revoked")
	}
}

func TestAllSnapshots(t *testing.T) {
	db := open(t)
	now := time.Now().Unix()
	if err := db.AddDevice("d1", "dev", []byte("pub"), now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("s1", "d1", now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("s2", "d1", now); err != nil {
		t.Fatal(err)
	}

	// empty: no snapshots yet
	refs, err := db.AllSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected 0 snapshots, got %d", len(refs))
	}

	// add one snapshot per share
	if err := db.AddSnapshot(Snapshot{ID: "snap1", Share: "s1", DeviceID: "d1", ManifestHash: "snap1", CreatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSnapshot(Snapshot{ID: "snap2", Share: "s2", DeviceID: "d1", ManifestHash: "snap2", CreatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}

	refs, err = db.AllSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(refs))
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.ID] = r.Share
	}
	if got["snap1"] != "s1" || got["snap2"] != "s2" {
		t.Fatalf("unexpected refs: %+v", got)
	}
}
