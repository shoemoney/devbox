package syncer

import (
	"net/http/httptest"
	"strings"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
)

// Regression for the critical data-loss bug: a hub edit to a path that the local
// device .devignore's (so it's filtered out of `ours`) must NOT silently clobber
// the on-disk file — its bytes are preserved as a conflict copy first.
func TestPullPreservesIgnoredLocalFile(t *testing.T) {
	db, _ := meta.Open(":memory:")
	defer db.Close()
	store, _ := blobstore.NewDisk(t.TempDir())
	srv := httptest.NewServer(hub.NewServer(db, store).Handler())
	defer srv.Close()
	guard, _ := secret.New(nil)
	igEmpty, _ := LoadIgnore(t.TempDir())

	A := joinDevice(t, db, srv.URL, "alice")
	B := joinDevice(t, db, srv.URL, "bob")
	if err := A.Publish("s"); err != nil {
		t.Fatal(err)
	}
	rootA := t.TempDir()

	// snap1 (B's base): the file exists on the hub.
	writeFile(t, rootA, "ignored.dat", "hub-v1\n")
	writeFile(t, rootA, "app.txt", "x\n")
	snap1, _, err := Sync(A, rootA, "s", "", "", "alice", 1, igEmpty, guard, nil)
	if err != nil {
		t.Fatalf("A sync1: %v", err)
	}
	// snap2 (head): A edits the file.
	writeFile(t, rootA, "ignored.dat", "hub-edit\n")
	if _, _, err = Sync(A, rootA, "s", "", snap1, "alice", 2, igEmpty, guard, nil); err != nil {
		t.Fatalf("A sync2: %v", err)
	}

	// B has the file on disk with precious LOCAL bytes, but .devignore's it.
	rootB := t.TempDir()
	writeFile(t, rootB, "ignored.dat", "mine-local\n")
	writeFile(t, rootB, ".devignore", "ignored.dat\n")
	igB, err := LoadIgnore(rootB)
	if err != nil {
		t.Fatal(err)
	}

	// B pulls from base=snap1; the hub's edit lands on a path B filters out.
	pr, err := Pull(B, rootB, "s", "", snap1, "bob", 3, igB, guard, nil)
	if err != nil {
		t.Fatalf("B pull: %v", err)
	}

	// The local bytes must survive as a conflict copy (no byte lost). The copy is
	// named with the conflict marker inserted before the extension, e.g.
	// "ignored.conflict-bob-3.dat".
	var conflict string
	for _, c := range pr.Conflicts {
		if strings.Contains(c, ".conflict-") && read(t, rootB, c) == "mine-local\n" {
			conflict = c
		}
	}
	if conflict == "" {
		t.Fatalf("ignored file's local bytes were clobbered (no conflict copy preserved them). conflicts=%v", pr.Conflicts)
	}
}
