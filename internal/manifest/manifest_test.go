package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/ignore"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildTree(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "main.go", "package main\n")
	write(t, root, "README.md", "# hi\n")
	write(t, root, "sub/code.go", "package sub\n")
	write(t, root, "node_modules/dep.js", "module.exports = 1\n") // ignored
	write(t, root, "sub/app.log", "noise\n")                      // ignored (*.log)
	write(t, root, ".env", "SECRET=1\n")                          // secret-blocked
	write(t, root, ".env.example", "SECRET=\n")                   // allowed
	return root
}

func compile(t *testing.T) (*ignore.Matcher, *secret.Guard) {
	t.Helper()
	ig, err := ignore.Compile([]string{"node_modules/", "*.log"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := secret.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ig, g
}

func TestBuildFiltersIgnoredAndSecrets(t *testing.T) {
	root := buildTree(t)
	ig, g := compile(t)

	m, blocked, err := Build(root, ig, g)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, e := range m.Entries {
		got[e.Path] = true
		if len(e.Chunks) == 0 {
			t.Errorf("%s has no chunks", e.Path)
		}
	}
	want := []string{".env.example", "README.md", "main.go", "sub/code.go"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected %q in manifest, missing", w)
		}
	}
	for _, bad := range []string{"node_modules/dep.js", "sub/app.log", ".env"} {
		if got[bad] {
			t.Errorf("%q should NOT be in manifest", bad)
		}
	}
	if len(m.Entries) != len(want) {
		t.Errorf("entry count = %d, want %d (%v)", len(m.Entries), len(want), m.Entries)
	}
	if len(blocked) != 1 || blocked[0] != ".env" {
		t.Errorf("blocked = %v, want [.env]", blocked)
	}
}

func TestManifestIDStable(t *testing.T) {
	root := buildTree(t)
	ig, g := compile(t)
	a, _, _ := Build(root, ig, g)
	b, _, _ := Build(root, ig, g)
	if a.ID() == "" {
		t.Fatal("empty manifest ID")
	}
	if a.ID() != b.ID() {
		t.Fatalf("manifest ID not stable: %s != %s", a.ID(), b.ID())
	}
}

func TestDiff(t *testing.T) {
	root := buildTree(t)
	ig, g := compile(t)
	old, _, _ := Build(root, ig, g)

	write(t, root, "sub/code.go", "package sub\n// changed\n") // modify
	write(t, root, "new.txt", "fresh\n")                       // add
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	} // delete

	cur, _, _ := Build(root, ig, g)
	ch := Diff(old, cur)

	if ch.Empty() {
		t.Fatal("expected changes")
	}
	if len(ch.Modified) != 1 || ch.Modified[0] != "sub/code.go" {
		t.Errorf("Modified = %v, want [sub/code.go]", ch.Modified)
	}
	if len(ch.Added) != 1 || ch.Added[0] != "new.txt" {
		t.Errorf("Added = %v, want [new.txt]", ch.Added)
	}
	if len(ch.Deleted) != 1 || ch.Deleted[0] != "README.md" {
		t.Errorf("Deleted = %v, want [README.md]", ch.Deleted)
	}
	if old.ID() == cur.ID() {
		t.Error("manifest ID should change after edits")
	}
}
