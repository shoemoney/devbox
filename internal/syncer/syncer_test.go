package syncer

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPushEndToEnd exercises the full M2 stack in-process:
// hub server + transport client + syncer, join -> publish -> push.
func TestPushEndToEnd(t *testing.T) {
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

	const tok = "jointoken"
	if err := db.CreateToken(hub.HashToken(tok), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}

	// A device tree: 2 real files, a synced .devignore, a secret, and ignored junk.
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "sub/util.go", "package sub\n")
	writeFile(t, root, ".devignore", "node_modules/\n")
	writeFile(t, root, ".env", "SECRET=1\n")            // secret -> blocked
	writeFile(t, root, "node_modules/x.js", "junk()\n") // ignored

	c := transport.New(srv.URL)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(tok, "testdev", pub); err != nil {
		t.Fatalf("join: %v", err)
	}
	if c.Bearer() == "" {
		t.Fatal("join did not set a bearer")
	}
	if err := c.Publish("projects"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ig, err := LoadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := secret.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Push(c, root, "projects", ig, guard, "")
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	// .devignore + main.go + sub/util.go = 3 synced files; node_modules excluded.
	if res.Files != 3 {
		t.Fatalf("Files = %d, want 3 (node_modules must be ignored)", res.Files)
	}
	if len(res.Blocked) != 1 || res.Blocked[0] != ".env" {
		t.Fatalf("Blocked = %v, want [.env]", res.Blocked)
	}
	if res.Snapshot == "" {
		t.Fatal("empty snapshot id")
	}
	head, err := c.Head("projects")
	if err != nil {
		t.Fatal(err)
	}
	if head != res.Snapshot {
		t.Fatalf("hub head %q != pushed snapshot %q", head, res.Snapshot)
	}

	// Second push of the unchanged tree: dedup means zero blobs uploaded and the
	// same content-addressed snapshot id.
	res2, err := Push(c, root, "projects", ig, guard, res.Head)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if res2.Uploaded != 0 {
		t.Fatalf("second push uploaded %d blobs, want 0 (hub already has them)", res2.Uploaded)
	}
	if res2.Snapshot != res.Snapshot {
		t.Fatalf("identical tree gave different snapshot: %q vs %q", res2.Snapshot, res.Snapshot)
	}
}
