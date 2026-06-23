// Package syncer drives the device-side push: build a content-addressed manifest
// of a tree (filtered by .devignore and the secret guard), upload only the blobs
// the hub is missing, and commit a snapshot.
package syncer

import (
	"os"
	"path/filepath"
	"strings"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
	"git.shoemoney.ai/shoemoney/devbox/internal/ignore"
	"git.shoemoney.ai/shoemoney/devbox/internal/manifest"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// Result summarizes a push.
type Result struct {
	Snapshot string
	Head     string
	Blocked  []string // secret files refused upload
	Uploaded int      // blobs actually uploaded (the hub lacked)
	Files    int      // files in the manifest
}

// Push builds a manifest of root (filtered by ig + guard), uploads any blobs the
// hub is missing (chunks + the manifest blob), then commits a snapshot on share.
// parent is the device's last-known head ("" if none).
func Push(c *transport.Client, root, share string, ig *ignore.Matcher, guard *secret.Guard, parent string) (Result, error) {
	m, blocked, err := manifest.Build(root, ig, guard)
	if err != nil {
		return Result{}, err
	}

	// Gather the bytes of every distinct chunk, plus the manifest blob itself.
	blobs := map[string][]byte{}
	refs := map[string]int64{} // distinct chunk hash -> size
	for _, e := range m.Entries {
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
	manBytes, err := m.Marshal()
	if err != nil {
		return Result{}, err
	}
	manHash := m.ID()
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
	for _, h := range missing {
		if err := c.PutBlob(h, blobs[h]); err != nil {
			return Result{}, err
		}
	}

	chunks := make([]proto.ChunkRef, 0, len(refs))
	for h, sz := range refs {
		chunks = append(chunks, proto.ChunkRef{Hash: h, Size: sz})
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
	return Result{
		Snapshot: resp.Snapshot,
		Head:     resp.Head,
		Blocked:  blocked,
		Uploaded: len(missing),
		Files:    len(m.Entries),
	}, nil
}

// LoadIgnore compiles root/.devignore (an empty matcher if the file is absent).
func LoadIgnore(root string) (*ignore.Matcher, error) {
	b, err := os.ReadFile(filepath.Join(root, ".devignore"))
	if os.IsNotExist(err) {
		return ignore.Compile(nil)
	}
	if err != nil {
		return nil, err
	}
	return ignore.Compile(strings.Split(string(b), "\n"))
}
