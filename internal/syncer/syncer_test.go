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
	"git.shoemoney.ai/shoemoney/devbox/internal/manifest"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
)

// joinDevice enrolls a fresh device against srv with a freshly-minted token.
func joinDevice(t *testing.T, db *meta.DB, baseURL, name string) *transport.Client {
	t.Helper()
	tok := name + "-tok"
	if err := db.CreateToken(hub.HashToken(tok), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	c := transport.New(baseURL)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(tok, name, pub); err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	return c
}

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

	res, err := Push(c, root, "projects", "", ig, guard, "", nil)
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

	// Pull foundation: the manifest blob downloads and parses back to 3 entries.
	manBytes, err := c.GetBlob(res.Snapshot)
	if err != nil {
		t.Fatalf("get manifest blob: %v", err)
	}
	gotManifest, err := manifest.Unmarshal(manBytes)
	if err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(gotManifest.Entries) != 3 {
		t.Fatalf("downloaded manifest has %d entries, want 3", len(gotManifest.Entries))
	}

	// Second push of the unchanged tree: dedup means zero blobs uploaded and the
	// same content-addressed snapshot id.
	res2, err := Push(c, root, "projects", "", ig, guard, res.Head, nil)
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

// TestPushConflictWhenBehind proves the M3 invariant: a device that is behind the
// share head cannot clobber it — the hub returns a conflict and keeps the head.
func TestPushConflictWhenBehind(t *testing.T) {
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
	ig, _ := LoadIgnore(t.TempDir()) // empty matcher

	// Device A publishes and pushes treeA -> head = snapA.
	a := joinDevice(t, db, srv.URL, "alice")
	if err := a.Publish("shared"); err != nil {
		t.Fatal(err)
	}
	rootA := t.TempDir()
	writeFile(t, rootA, "a.txt", "from alice\n")
	resA, err := Push(a, rootA, "shared", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("A push: %v", err)
	}
	if resA.Conflict {
		t.Fatal("A's first push should not conflict")
	}

	// Device B, unaware of A's snapshot, pushes a different tree with parent="".
	b := joinDevice(t, db, srv.URL, "bob")
	rootB := t.TempDir()
	writeFile(t, rootB, "b.txt", "from bob\n")
	resB, err := Push(b, rootB, "shared", "", ig, guard, "", nil)
	if err != nil {
		t.Fatalf("B push: %v", err)
	}
	if !resB.Conflict {
		t.Fatal("B is behind head and must get a conflict, not clobber A's head")
	}
	if resB.Head != resA.Snapshot {
		t.Fatalf("conflict head = %q, want A's snapshot %q", resB.Head, resA.Snapshot)
	}

	// A's head must be intact — B did not clobber it.
	head, err := a.Head("shared")
	if err != nil {
		t.Fatal(err)
	}
	if head != resA.Snapshot {
		t.Fatalf("head clobbered: %q, want %q", head, resA.Snapshot)
	}
}
