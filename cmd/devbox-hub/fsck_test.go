package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/hub"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/secret"
	"github.com/shoemoney/devbox/internal/syncer"
	"github.com/shoemoney/devbox/internal/transport"
)

// TestCheckDanglingHealthy asserts a clean store reports zero dangling snapshots.
func TestCheckDanglingHealthy(t *testing.T) {
	db, store, _ := fsckTestSetup(t, "proj")
	dangling, err := checkDangling(db, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(dangling) != 0 {
		t.Fatalf("healthy store: want 0 dangling, got %d: %+v", len(dangling), dangling)
	}
}

// TestCheckDanglingMissingChunk asserts that deleting a chunk blob from the
// store causes the snapshot referencing it to be reported as dangling.
func TestCheckDanglingMissingChunk(t *testing.T) {
	db, store, snap := fsckTestSetup(t, "proj")

	// grab all chunk hashes this snapshot references
	chunks := manifestChunksOrFail(t, store, snap)
	if len(chunks) == 0 {
		t.Fatal("test setup: snapshot has no chunks")
	}
	victim := chunks[0]

	// delete the chunk blob directly from the store
	if err := store.Delete(victim); err != nil {
		t.Fatalf("deleting chunk blob: %v", err)
	}

	dangling, err := checkDangling(db, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(dangling) != 1 {
		t.Fatalf("want 1 dangling snapshot, got %d: %+v", len(dangling), dangling)
	}
	d := dangling[0]
	if d.Share != "proj" {
		t.Fatalf("dangling.Share = %q, want %q", d.Share, "proj")
	}
	if d.ID != snap {
		t.Fatalf("dangling.ID = %q, want %q", d.ID, snap)
	}
	found := false
	for _, m := range d.Missing {
		if m == victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing list %v does not contain deleted chunk %s", d.Missing, victim)
	}
}

// TestCheckDanglingMissingManifest asserts that deleting the manifest blob
// itself causes the snapshot to be reported as dangling.
func TestCheckDanglingMissingManifest(t *testing.T) {
	db, store, snap := fsckTestSetup(t, "proj")

	if err := store.Delete(snap); err != nil {
		t.Fatalf("deleting manifest blob: %v", err)
	}

	dangling, err := checkDangling(db, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(dangling) != 1 {
		t.Fatalf("want 1 dangling snapshot, got %d", len(dangling))
	}
	found := false
	for _, m := range dangling[0].Missing {
		if m == snap {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing list %v does not contain manifest hash %s", dangling[0].Missing, snap)
	}
}

// fsckTestSetup spins up a hub, enrolls a device, pushes one snapshot, and
// returns the db, store, and snapshot id. Cleans up on test end.
func fsckTestSetup(t *testing.T, share string) (*meta.DB, blobstore.Store, string) {
	t.Helper()
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	t.Cleanup(srv.Close)

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
	if err := c.Publish(share); err != nil {
		t.Fatal(err)
	}

	guard, _ := secret.New(nil)
	root := t.TempDir()
	ig, _ := syncer.LoadIgnore(root)

	writeFile(t, root, "file.txt", "some content for fsck test\n")
	result, err := syncer.Push(c, root, share, "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	return db, store, result.Snapshot
}
