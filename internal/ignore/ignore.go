// Package ignore implements a .devignore matcher with gitignore-compatible
// semantics (last-match-wins, negation, anchoring, ** globbing).
package ignore

import (
	"regexp"
	"strings"
)

// Matcher matches forward-slash paths (relative to the project root) against a
// compiled set of .devignore patterns.
type Matcher struct {
	rules []rule
}

type rule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Compile parses the lines of a .devignore file into a Matcher.
func Compile(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, p := range patterns {
		r, ok, err := compilePattern(p)
		if err != nil {
			return nil, err
		}
		if ok {
			m.rules = append(m.rules, r)
		}
	}
	return m, nil
}

func compilePattern(line string) (rule, bool, error) {
	// Trim trailing whitespace; gitignore keeps leading whitespace literal.
	line = strings.TrimRight(line, " \t\r\n")
	if line == "" || strings.HasPrefix(line, "#") {
		return rule{}, false, nil
	}

	var r rule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// A pattern is anchored to root if it contains a (non-trailing) slash or
	// has a leading slash; otherwise a bare name matches at any depth.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")

	re, err := regexp.Compile(buildRegex(line, anchored))
	if err != nil {
		return rule{}, false, err
	}
	r.re = re
	return r, true, nil
}

// buildRegex translates a gitignore glob into an anchored regexp matching a
// whole path. When unanchored, the pattern may begin at any path segment.
func buildRegex(glob string, anchored bool) string {
	var b strings.Builder
	b.WriteString("^")
	if anchored {
		b.WriteString(globToRegex(glob))
	} else {
		// Bare name: match at root or after any "/".
		b.WriteString("(?:.*/)?")
		b.WriteString(globToRegex(glob))
	}
	b.WriteString("$")
	return b.String()
}

// globToRegex converts gitignore glob metacharacters to a regexp fragment.
//   - `**` spans segments (including slashes)
//   - `*` matches within a single segment (no slash)
//   - `?` matches one non-slash char
func globToRegex(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				b.WriteString(".*") // ponytail: treat ** as "anything incl. /"
				i++
				// swallow a slash directly after ** so a/**/b matches a/b
				if i+1 < len(g) && g[i+1] == '/' {
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// Match reports whether path (forward-slash, relative to root) is ignored.
// isDir indicates whether path is a directory.
func (m *Matcher) Match(path string, isDir bool) bool {
	path = strings.Trim(path, "/")

	// .devbox is always ignored, including everything under it.
	if path == ".devbox" || strings.HasPrefix(path, ".devbox/") {
		return true
	}

	// A path is ignored if itself matches, or if any ancestor directory is
	// ignored (last-match-wins is evaluated per candidate path).
	if m.matchOne(path, isDir) {
		return true
	}
	segs := strings.Split(path, "/")
	for i := 1; i < len(segs); i++ {
		if m.matchOne(strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return false
}

// matchOne applies last-match-wins to a single path (no ancestor walking).
func (m *Matcher) matchOne(path string, isDir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(path) {
			ignored = !r.negate
		}
	}
	return ignored
}
