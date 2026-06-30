// Package syncer drives the device-side push: build a content-addressed manifest
// of a tree (filtered by .devignore and the secret guard), upload only the blobs
// the hub is missing, and commit a snapshot.
package syncer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/internal/hooks"
	"github.com/shoemoney/devbox/internal/ignore"
	"github.com/shoemoney/devbox/internal/manifest"
	"github.com/shoemoney/devbox/internal/secret"
	"github.com/shoemoney/devbox/internal/transport"
	"github.com/shoemoney/devbox/pkg/proto"
)

// Result summarizes a push.
type Result struct {
	Snapshot string
	Head     string
	Blocked  []string // secret files refused upload
	Uploaded int      // blobs actually uploaded (the hub lacked)
	Files    int      // files in the manifest
	Conflict bool     // hub rejected the push: device is behind head (pull + reconcile)
	Vetoed   bool     // a pre-push hook aborted the push
}

// Push builds a manifest of root (filtered by ig + guard), uploads any blobs the
// hub is missing (chunks + the manifest blob), then commits a snapshot on share.
// For a sub-path mount (subpath != "") root maps to share/subpath/: the local
// subtree is spliced into the share head (entries outside subpath are preserved)
// so a partial mount can push without dropping the rest of the share.
// parent is the device's last-known head ("" if none).
func Push(c *transport.Client, root, share, subpath string, ig *ignore.Matcher, guard *secret.Guard, parent string, hk *hooks.Runner) (Result, error) {
	localM, blocked, err := manifest.Build(root, ig, guard)
	if err != nil {
		return Result{}, err
	}

	paths := make([]string, len(localM.Entries))
	for i, e := range localM.Entries {
		paths[i] = e.Path
	}
	if err := hk.Run(hooks.PrePush, paths, parent); err != nil {
		return Result{Vetoed: true}, nil // pre-push hook vetoed; skip this push
	}

	// Gather the bytes of every distinct local chunk (with sizes).
	blobs := map[string][]byte{}
	refs := map[string]int64{} // local chunk hash -> size
	for _, e := range localM.Entries {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			return Result{}, err
		}
		cs, err := chunk.Split(data)
		if err != nil {
			return Result{}, err
		}
		for _, ch := range cs {
			if _, ok := blobs[ch.Hash]; !ok {
				blobs[ch.Hash] = data[ch.Offset : ch.Offset+ch.Size]
				refs[ch.Hash] = ch.Size
			}
		}
	}

	// Splice the local subtree into the full share manifest (no-op when subpath == "").
	fullM, err := splice(c, localM, subpath, parent)
	if err != nil {
		return Result{}, err
	}
	manBytes, err := fullM.Marshal()
	if err != nil {
		return Result{}, err
	}
	manHash := fullM.ID()
	blobs[manHash] = manBytes

	// Upload only what the hub lacks.
	all := make([]string, 0, len(blobs))
	for h := range blobs {
		all = append(all, h)
	}
	missing, err := c.Have(all)
	if err != nil {
		return Result{}, err
	}
	if err := uploadBlobs(c, missing, blobs); err != nil {
		return Result{}, err
	}

	// Declare every distinct chunk the full manifest references so the hub's
	// refcounts stay correct. Local chunks carry their size; chunks already on
	// the hub (outside the subpath) carry 0 — the hub keeps the size it recorded
	// on first upload and just bumps the refcount.
	seen := map[string]bool{}
	var chunks []proto.ChunkRef
	for _, e := range fullM.Entries {
		for _, h := range e.Chunks {
			if seen[h] {
				continue
			}
			seen[h] = true
			chunks = append(chunks, proto.ChunkRef{Hash: h, Size: refs[h]})
		}
	}

	resp, err := c.Push(proto.PushRequest{
		Share:        share,
		Parent:       parent,
		ManifestHash: manHash,
		Chunks:       chunks,
	})
	if err != nil {
		return Result{}, err
	}
	if !resp.Conflict {
		_ = hk.Run(hooks.PostPush, paths, resp.Snapshot) // best-effort; never aborts
	}
	return Result{
		Snapshot: resp.Snapshot,
		Head:     resp.Head,
		Blocked:  blocked,
		Uploaded: len(missing),
		Files:    len(localM.Entries),
		Conflict: resp.Conflict,
	}, nil
}

// splice builds the full share manifest from a sub-path mount's local manifest:
// it re-prefixes local entries with subpath and keeps the parent snapshot's
// entries outside subpath. For a whole-share mount it returns localM unchanged.
func splice(c *transport.Client, localM manifest.Manifest, subpath, parent string) (manifest.Manifest, error) {
	if subpath == "" {
		return localM, nil
	}
	entries := make([]manifest.Entry, 0, len(localM.Entries))
	if parent != "" {
		base, err := fetchManifest(c, parent)
		if err != nil {
			return manifest.Manifest{}, err
		}
		for _, e := range base.Entries {
			if !under(e.Path, subpath) {
				entries = append(entries, e)
			}
		}
	}
	for _, e := range localM.Entries {
		e.Path = prefixPath(e.Path, subpath)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return manifest.Manifest{Entries: entries}, nil
}

// uploadConcurrency bounds parallel blob uploads. WAN round-trip latency, not
// bandwidth, dominates a push of many small chunks; a small pool keeps the pipe
// full while the shared rate limiter still caps total throughput.
const uploadConcurrency = 8

// uploadBlobs PUTs every missing blob, up to uploadConcurrency in flight, and
// returns the first error (cancelling further starts). Order doesn't matter —
// blobs are content-addressed and independent.
func uploadBlobs(c *transport.Client, missing []string, blobs map[string][]byte) error {
	sem := make(chan struct{}, uploadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, h := range missing {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.PutBlob(h, blobs[h]); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	return firstErr
}

// LoadIgnore compiles root/.devignore (an empty matcher if the file is absent).
func LoadIgnore(root string) (*ignore.Matcher, error) {
	return LoadIgnoreWith(root, nil)
}

// LoadIgnoreWith compiles root/.devignore plus extra device-local patterns
// (gitignore syntax) appended after it — so a per-mount --exclude layers on top
// of the shared ignore file with last-match-wins precedence.
func LoadIgnoreWith(root string, extra []string) (*ignore.Matcher, error) {
	var lines []string
	b, err := os.ReadFile(filepath.Join(root, ".devignore"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		lines = strings.Split(string(b), "\n")
	}
	lines = append(lines, extra...)
	return ignore.Compile(lines)
}
