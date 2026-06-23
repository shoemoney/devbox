// Package manifest builds and diffs the synced state of a tree: the set of
// regular files (filtered by .devignore and the secret guard) mapped to their
// content chunks. A manifest is content-addressed; its ID identifies a snapshot.
package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
	"git.shoemoney.ai/shoemoney/devbox/internal/ignore"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
)

// Entry is one regular file in a manifest. Path is forward-slash, relative to root.
type Entry struct {
	Path   string
	Mode   uint32
	Size   int64
	Chunks []string
}

// Manifest is the sorted set of files comprising a tree's synced state.
type Manifest struct {
	Entries []Entry
}

// Build walks root, skipping .devignore matches and (separately) secret-guard
// matches, chunks each remaining regular file, and returns the manifest plus the
// sorted list of secret paths that were blocked from sync. ig and guard may be nil.
func Build(root string, ig *ignore.Matcher, guard *secret.Guard) (Manifest, []string, error) {
	var entries []Entry
	var blocked []string

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if ig != nil && ig.Match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // ponytail: symlinks/specials unsupported in v1
		}
		if ig != nil && ig.Match(rel, false) {
			return nil
		}
		if guard != nil && guard.Blocked(rel) {
			blocked = append(blocked, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) // ponytail: whole-file read; stream huge files later
		if err != nil {
			return err
		}
		var hashes []string
		for _, c := range chunk.Split(data) {
			hashes = append(hashes, c.Hash)
		}
		entries = append(entries, Entry{
			Path:   rel,
			Mode:   uint32(info.Mode().Perm()),
			Size:   info.Size(),
			Chunks: hashes,
		})
		return nil
	})
	if err != nil {
		return Manifest{}, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Strings(blocked)
	return Manifest{Entries: entries}, blocked, nil
}

// ID is the content address of the manifest (BLAKE3 of its canonical form).
func (m Manifest) ID() string {
	var b strings.Builder
	for _, e := range m.Entries {
		fmt.Fprintf(&b, "%s\x00%o\x00%d\x00%s\n", e.Path, e.Mode, e.Size, strings.Join(e.Chunks, ","))
	}
	return chunk.Hash([]byte(b.String()))
}

// Changes is the difference between two manifests.
type Changes struct {
	Added    []string
	Modified []string
	Deleted  []string
}

// Empty reports whether there are no changes.
func (c Changes) Empty() bool {
	return len(c.Added)+len(c.Modified)+len(c.Deleted) == 0
}

// Diff computes what changed from old to cur (by path; modified = different
// chunk list, size, or mode).
func Diff(old, cur Manifest) Changes {
	om := index(old)
	cm := index(cur)
	var ch Changes
	for path, ce := range cm {
		oe, ok := om[path]
		if !ok {
			ch.Added = append(ch.Added, path)
		} else if !sameContent(oe, ce) {
			ch.Modified = append(ch.Modified, path)
		}
	}
	for path := range om {
		if _, ok := cm[path]; !ok {
			ch.Deleted = append(ch.Deleted, path)
		}
	}
	sort.Strings(ch.Added)
	sort.Strings(ch.Modified)
	sort.Strings(ch.Deleted)
	return ch
}

func index(m Manifest) map[string]Entry {
	idx := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		idx[e.Path] = e
	}
	return idx
}

func sameContent(a, b Entry) bool {
	if a.Size != b.Size || a.Mode != b.Mode || len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for i := range a.Chunks {
		if a.Chunks[i] != b.Chunks[i] {
			return false
		}
	}
	return true
}
