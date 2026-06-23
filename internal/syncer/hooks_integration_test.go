package syncer

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/hooks"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
)

func writeExecHook(t *testing.T, root, event, script string) {
	t.Helper()
	dir := filepath.Join(root, ".devbox", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, event), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func quietRunner(root, share string) *hooks.Runner {
	r := hooks.New(root, share, "host", "remote")
	r.Stdout, r.Stderr = io.Discard, io.Discard
	return r
}

func TestPostPullHookFires(t *testing.T) {
	db, _ := meta.Open(":memory:")
	defer db.Close()
	store, _ := blobstore.NewDisk(t.TempDir())
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()
	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	A := joinDevice(t, db, srv.URL, "alice")
	B := joinDevice(t, db, srv.URL, "bob")
	_ = A.Publish("s")
	rootA := t.TempDir()
	writeFile(t, rootA, "app.txt", "v1\n")
	if _, _, err := Sync(A, rootA, "s", "", "", "alice", 1, ig, guard, nil); err != nil {
		t.Fatal(err)
	}

	rootB := t.TempDir()
	marker := filepath.Join(rootB, "hook-ran.txt")
	writeExecHook(t, rootB, "post-pull", "#!/usr/bin/env bash\ncp \"$DEVBOX_CHANGED_FILES\" '"+marker+"'\n")
	if _, _, err := Sync(B, rootB, "s", "", "", "bob", 2, ig, guard, quietRunner(rootB, "s")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("post-pull hook did not run: %v", err)
	}
	if !strings.Contains(string(b), "app.txt") {
		t.Fatalf("post-pull DEVBOX_CHANGED_FILES = %q, want it to list app.txt", string(b))
	}
}

func TestPrePushHookVetoes(t *testing.T) {
	db, _ := meta.Open(":memory:")
	defer db.Close()
	store, _ := blobstore.NewDisk(t.TempDir())
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()
	guard, _ := secret.New(nil)
	ig, _ := LoadIgnore(t.TempDir())

	C := joinDevice(t, db, srv.URL, "carol")
	_ = C.Publish("s")
	rootC := t.TempDir()
	writeFile(t, rootC, "x.txt", "hi\n")
	writeExecHook(t, rootC, "pre-push", "#!/usr/bin/env bash\nexit 1\n")

	res, err := Push(C, rootC, "s", "", ig, guard, "", quietRunner(rootC, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Vetoed {
		t.Fatal("a pre-push hook exiting non-zero must veto the push")
	}
	if h, _ := C.Head("s"); h != "" {
		t.Fatalf("a vetoed push must not advance head, got %q", h)
	}
}
