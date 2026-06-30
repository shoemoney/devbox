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

// TestGCBothSharesPruneSameSnapshot is the missing case: BOTH shares advance past
// the shared snapshot S, so S is a prunable entry in BOTH shares simultaneously.
// The pre-fix code reads the manifest blob inline per prunable entry — the first
// deletion removes the blob so the second entry gets ErrNotFound → nil hashes →
// chunk refcount never decremented → permanent storage leak.
// After the fix the chunk list is read once up-front, so both decrements happen
// correctly and the chunk drops to refcount 0 and is deleted.
func TestGCBothSharesPruneSameSnapshot(t *testing.T) {
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
	if err := c.Publish("a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("b"); err != nil {
		t.Fatal(err)
	}
	guard, _ := secret.New(nil)

	// Push byte-identical content to BOTH shares → same snapshot id S, chunks refcount 2.
	rootA, rootB := t.TempDir(), t.TempDir()
	igA, _ := syncer.LoadIgnore(rootA)
	igB, _ := syncer.LoadIgnore(rootB)
	writeFile(t, rootA, "f.txt", "identical bytes\n")
	writeFile(t, rootB, "f.txt", "identical bytes\n")
	sa, err := syncer.Push(c, rootA, "a", "", igA, guard, "", nil)
	if err != nil {
		t.Fatalf("push a: %v", err)
	}
	sb, err := syncer.Push(c, rootB, "b", "", igB, guard, "", nil)
	if err != nil {
		t.Fatalf("push b: %v", err)
	}
	if sa.Snapshot != sb.Snapshot {
		t.Fatalf("identical content must collide: a=%s b=%s", sa.Snapshot, sb.Snapshot)
	}
	sharedChunks := manifestChunksOrFail(t, store, sa.Snapshot)
	if len(sharedChunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// Advance BOTH shares past S → S is prunable in BOTH, not the live head of either.
	writeFile(t, rootA, "f.txt", "changed on a\n")
	if _, err := syncer.Push(c, rootA, "a", "", igA, guard, sa.Head, nil); err != nil {
		t.Fatalf("advance a: %v", err)
	}
	writeFile(t, rootB, "f.txt", "changed on b\n")
	if _, err := syncer.Push(c, rootB, "b", "", igB, guard, sb.Head, nil); err != nil {
		t.Fatalf("advance b: %v", err)
	}

	if _, _, err := runGC(db, store, 1, 0, false); err != nil {
		t.Fatalf("gc: %v", err)
	}

	// S's manifest blob and ALL its chunks must be gone — no share's live head is S.
	if has, _ := store.Has(sa.Snapshot); has {
		t.Fatal("snapshot S manifest blob should be deleted (no share references it)")
	}
	for _, h := range sharedChunks {
		if has, _ := store.Has(h); has {
			t.Fatalf("chunk %s leaked: still in blobstore after both shares advanced past S", h)
		}
		if rc, _ := db.ChunkRefcount(h); rc != 0 {
			t.Fatalf("chunk %s refcount = %d, want 0 (double-prune leak)", h, rc)
		}
	}
}

// Two shares with byte-identical content collide on the same content-addressed
// snapshot id (the idempotent push skips the refcount bump). Advancing one share
// makes that id "deletable" for it — but it's still the OTHER share's live head.
// GC must NOT delete the chunks/manifest that head needs. Regression for the
// critical cross-share refcount/GC data-loss finding.
func TestGCKeepsChunksSharedAcrossShares(t *testing.T) {
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
	if err := c.Publish("a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("b"); err != nil {
		t.Fatal(err)
	}
	guard, _ := secret.New(nil)

	// Push byte-identical content to BOTH shares -> same snapshot id S.
	rootA, rootB := t.TempDir(), t.TempDir()
	igA, _ := syncer.LoadIgnore(rootA)
	igB, _ := syncer.LoadIgnore(rootB)
	writeFile(t, rootA, "f.txt", "identical bytes\n")
	writeFile(t, rootB, "f.txt", "identical bytes\n")
	sa, err := syncer.Push(c, rootA, "a", "", igA, guard, "", nil)
	if err != nil {
		t.Fatalf("push a: %v", err)
	}
	sb, err := syncer.Push(c, rootB, "b", "", igB, guard, "", nil)
	if err != nil {
		t.Fatalf("push b: %v", err)
	}
	if sa.Snapshot != sb.Snapshot {
		t.Fatalf("identical content should collide: a=%s b=%s", sa.Snapshot, sb.Snapshot)
	}
	shared := manifestChunksOrFail(t, store, sb.Snapshot) // b's head chunks
	if len(shared) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// Advance share a so S becomes deletable FOR A (b's head stays S).
	writeFile(t, rootA, "f.txt", "changed on a\n")
	if _, err := syncer.Push(c, rootA, "a", "", igA, guard, sa.Head, nil); err != nil {
		t.Fatalf("advance a: %v", err)
	}

	// GC keeping only each share's head.
	if _, _, err := runGC(db, store, 1, 0, false); err != nil {
		t.Fatalf("gc: %v", err)
	}

	// b's head, its manifest blob, and its chunks must ALL survive.
	if head, _, _ := db.ShareHead("b"); head != sb.Snapshot {
		t.Fatalf("share b head = %q after gc, want %q (clobbered!)", head, sb.Snapshot)
	}
	if has, _ := store.Has(sb.Snapshot); !has {
		t.Fatal("b's head manifest blob was deleted by gc (cross-share data loss)")
	}
	for _, h := range shared {
		if has, _ := store.Has(h); !has {
			t.Fatalf("chunk %s referenced by b's live head was deleted by gc (data loss)", h)
		}
	}
}
