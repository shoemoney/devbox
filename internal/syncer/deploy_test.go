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

// TestDeployPinsWithoutPushing pushes v1 then v2, then deploys v1 into a separate
// directory. Unlike Restore, Deploy must NOT advance the hub head: the deploy
// target ends up at v1's content while the share head stays at v2.
func TestDeployPinsWithoutPushing(t *testing.T) {
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

	c := joinDevice(t, db, srv.URL, "deployer")
	if err := c.Publish("app"); err != nil {
		t.Fatal(err)
	}

	// Author the share on one tree: v1, then v2.
	src := t.TempDir()
	writeFile(t, src, "index.html", "v1\n")
	writeFile(t, src, "app.js", "console.log(1)\n")
	snap1, err := Push(c, src, "app", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}
	writeFile(t, src, "index.html", "v2\n")
	writeFile(t, src, "new.css", "body{}\n")
	snap2, err := Push(c, src, "app", "", ig, guard, snap1.Head, nil)
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}

	// Deploy v1 into a fresh /var/www-style target that already has a stray file.
	target := t.TempDir()
	writeFile(t, target, "stale.txt", "leftover\n")
	res, err := Deploy(c, target, "", snap1.Snapshot, ig, guard)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Target matches v1 exactly: v1 files present, v2-only and stray files gone.
	if got := readFile(t, target, "index.html"); got != "v1\n" {
		t.Fatalf("index.html = %q, want v1", got)
	}
	if got := readFile(t, target, "app.js"); got != "console.log(1)\n" {
		t.Fatalf("app.js = %q, want v1", got)
	}
	if _, err := os.Stat(filepath.Join(target, "new.css")); !os.IsNotExist(err) {
		t.Fatal("new.css (v2-only) should be absent in a v1 deploy")
	}
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("stray stale.txt should be deleted to match the snapshot")
	}
	if res.Snapshot != snap1.Snapshot {
		t.Fatalf("deploy snapshot = %q, want v1 %q", res.Snapshot, snap1.Snapshot)
	}

	// The crux: deploy never pushes — the hub head is still v2.
	head, err := c.Head("app")
	if err != nil {
		t.Fatal(err)
	}
	if head != snap2.Snapshot {
		t.Fatalf("hub head after deploy = %q, want unchanged v2 %q", head, snap2.Snapshot)
	}
}
