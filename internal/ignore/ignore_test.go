package ignore

import (
	"strings"
	"testing"
)

const devignore = `
node_modules/
dist/
build/
.next/
target/
*.log
*.tmp
.DS_Store
.env
.env.*
*.pem
*.key
secrets/
!.env.example
`

func newMatcher(t *testing.T) *Matcher {
	t.Helper()
	m, err := Compile(strings.Split(devignore, "\n"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return m
}

func TestMatch(t *testing.T) {
	m := newMatcher(t)

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		// node_modules at any depth
		{"node_modules", true, true},
		{"node_modules/x", false, true},
		{"a/b/node_modules/c", false, true},

		// other dir patterns
		{"dist/x", false, true},
		{"build", true, true},
		{".next", true, true},
		{"target/y", false, true},

		// extension / name patterns at any depth
		{"app.log", false, true},
		{"a/b/c.tmp", false, true},
		{".DS_Store", false, true},
		{"sub/.DS_Store", false, true},

		// secrets + env + keys
		{".env", false, true},
		{".env.local", false, true},
		{"key.pem", false, true},
		{"id.key", false, true},
		{"secrets/aws", false, true},

		// negation beats .env.*
		{".env.example", false, false},

		// .devbox always ignored
		{".devbox/anything", false, true},
		{".devbox/hooks/post-pull", false, true},

		// not ignored
		{"src/main.go", false, false},
		{"README.md", false, false},
	}

	for _, c := range cases {
		if got := m.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}
