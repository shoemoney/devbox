package syncer

import (
	"strings"
	"testing"
)

// A hub-supplied manifest path must never resolve outside the mount root.
func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := "/srv/project"
	for _, p := range []string{
		"../escape.txt",
		"../../.ssh/authorized_keys",
		"/etc/passwd",
		"..",
		"a/../../b",
		"",
	} {
		if got, err := safeJoin(root, p); err == nil {
			t.Errorf("safeJoin(%q) = %q, nil — escape not blocked", p, got)
		}
	}
	for _, p := range []string{"a.txt", "sub/dir/file.go", "a/../b"} {
		got, err := safeJoin(root, p)
		if err != nil {
			t.Errorf("safeJoin(%q) = err %v, want ok", p, err)
		} else if !strings.HasPrefix(got, root) {
			t.Errorf("safeJoin(%q) = %q, escaped root", p, got)
		}
	}
}
