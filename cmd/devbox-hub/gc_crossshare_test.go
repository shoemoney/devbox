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
	if _, _, err := runGC(db, store, 1, false); err != nil {
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
