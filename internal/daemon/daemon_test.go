package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
)

func join(t *testing.T, db *meta.DB, url, name string) string {
	t.Helper()
	tok := name + "-tok"
	if err := db.CreateToken(hub.HashToken(tok), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	c := transport.New(url)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(tok, name, pub, priv); err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	return c.Bearer()
}

func writeF(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(path, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && string(b) == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestLiveTwoWaySyncViaSSE runs two daemons against one hub and proves a change
// on one machine reaches the other automatically (push -> SSE event -> pull).
func TestLiveTwoWaySyncViaSSE(t *testing.T) {
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
	mount := func(hubURL, bearer, root string) (string, config.Daemon) {
		return t.TempDir(), config.Daemon{
			Hub: hubURL, Bearer: bearer,
			Mounts: []config.Mount{{Share: "s", Local: root, Hub: hubURL}},
		}
	}
	dirA, cfgA := mount(srv.URL, bearerA, rootA)
	dirB, cfgB := mount(srv.URL, bearerB, rootB)

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
	time.Sleep(200 * time.Millisecond) // let both subscribe before the first edit

	// Alice creates a file -> it should reach Bob automatically.
	writeF(t, rootA, "hello.txt", "from alice\n")
	if !waitFor(filepath.Join(rootB, "hello.txt"), "from alice\n", 10*time.Second) {
		t.Fatal("alice's file did not reach bob via live sync")
	}

	// Bob creates a file -> it should reach Alice automatically.
	writeF(t, rootB, "reply.txt", "from bob\n")
	if !waitFor(filepath.Join(rootA, "reply.txt"), "from bob\n", 10*time.Second) {
		t.Fatal("bob's file did not reach alice via live sync")
	}
}
