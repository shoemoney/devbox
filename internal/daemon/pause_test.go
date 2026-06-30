package daemon

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/config"
	"github.com/shoemoney/devbox/internal/hub"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/transport"
)

// absent reports whether path stays absent for the whole window. It's the
// inverse of waitFor: it proves a change did NOT propagate.
func absent(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// TestPauseForAutoResumes proves PauseFor pauses immediately and clears the
// pause on its own after the duration, and that a plain Pause cancels a pending
// auto-resume (an explicit indefinite pause must override a scheduled resume).
func TestPauseForAutoResumes(t *testing.T) {
	d, err := New(t.TempDir(), config.Daemon{}, "h", t.Logf)
	if err != nil {
		t.Fatal(err)
	}

	d.PauseFor(40 * time.Millisecond)
	if !d.StateSnapshot().Paused {
		t.Fatal("PauseFor should pause immediately")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && d.StateSnapshot().Paused {
		time.Sleep(10 * time.Millisecond)
	}
	if d.StateSnapshot().Paused {
		t.Fatal("PauseFor did not auto-resume after its duration")
	}

	// A plain Pause after scheduling an auto-resume must stay paused indefinitely.
	d.PauseFor(40 * time.Millisecond)
	d.Pause() // cancels the pending 40ms auto-resume
	time.Sleep(200 * time.Millisecond)
	if !d.StateSnapshot().Paused {
		t.Fatal("plain Pause did not cancel the pending auto-resume")
	}
}

// TestPauseGatesSyncing is the load-bearing pause test: while paused, a local
// edit must NOT reach the peer; after resume it must. If Pause() were wired but
// runMount still synced through the trigger, the "absent while paused" assertion
// would fail — so this test fails loudly if pause doesn't gate syncing.
func TestPauseGatesSyncing(t *testing.T) {
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

	bearerA := join(t, db, srv.URL, "alice")
	bearerB := join(t, db, srv.URL, "bob")

	ca := transport.New(srv.URL)
	ca.SetBearer(bearerA)
	if err := ca.Publish("s"); err != nil {
		t.Fatal(err)
	}

	rootA, rootB := t.TempDir(), t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()
	cfgA := config.Daemon{Hub: srv.URL, Bearer: bearerA, Mounts: []config.Mount{{Share: "s", Local: rootA, Hub: srv.URL}}}
	cfgB := config.Daemon{Hub: srv.URL, Bearer: bearerB, Mounts: []config.Mount{{Share: "s", Local: rootB, Hub: srv.URL}}}

	dA, err := New(dirA, cfgA, "alice", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	dB, err := New(dirB, cfgB, "bob", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dA.Run(ctx)
	go dB.Run(ctx)
	time.Sleep(300 * time.Millisecond) // let both subscribe + do initial sync

	// Pause Alice, then make a local edit. It must NOT reach Bob while paused.
	dA.Pause()
	writeF(t, rootA, "while-paused.txt", "should not propagate yet\n")
	peerPath := filepath.Join(rootB, "while-paused.txt")
	if !absent(peerPath, 2*time.Second) {
		t.Fatal("edit propagated while paused — Pause() did not gate syncing")
	}

	// Resume Alice: the buffered edit must now converge to Bob.
	dA.Resume()
	if !waitFor(peerPath, "should not propagate yet\n", 10*time.Second) {
		t.Fatal("edit did not propagate after resume — Resume() did not catch up")
	}
}
