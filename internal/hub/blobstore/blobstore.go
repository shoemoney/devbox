// Package blobstore is devbox's content-addressed blob storage abstraction.
//
// Blobs (chunks and manifests) are keyed by their caller-supplied content hash.
// The disk implementation (v1) stores each blob at root/<first2>/<hash>; an
// S3/R2 implementation can satisfy the same Store interface for the hosted tier.
// The store is "dumb": it trusts the key is the content hash and does not verify
// — the hub layer verifies on ingest.
package blobstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned by Get/Delete for a missing blob.
var ErrNotFound = errors.New("blobstore: blob not found")

// safeKey rejects keys that aren't a single, separator-free filename. The hub
// validates hash *shape* at its HTTP boundary; this is defense in depth so a key
// like "../../etc/passwd" (or one whose first two chars are "..", which would
// escape via shardDir) can never leave the blob root, even if a future caller
// forgets to validate. Stays algorithm-agnostic (no fixed length/charset).
func safeKey(hash string) error {
	if hash == "" || hash == "." || strings.Contains(hash, "..") || strings.ContainsAny(hash, `/\`) {
		return fmt.Errorf("blobstore: unsafe blob key %q", hash)
	}
	return nil
}

// Store is a content-addressed blob store.
type Store interface {
	Has(hash string) (bool, error)      // does a blob with this hash exist?
	Put(hash string, data []byte) error // idempotent store
	Get(hash string) ([]byte, error)    // ErrNotFound if absent
	Delete(hash string) error           // ErrNotFound if absent; used by GC
}

// Disk is a filesystem-backed Store rooted at a directory.
type Disk struct{ root string }

// NewDisk opens (creating if needed) a disk blob store at root.
func NewDisk(root string) (*Disk, error) {
	if root == "" {
		return nil, fmt.Errorf("blobstore: empty root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: root}, nil
}

func (d *Disk) shardDir(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(d.root, "_short")
	}
	return filepath.Join(d.root, hash[:2])
}

func (d *Disk) path(hash string) string {
	return filepath.Join(d.shardDir(hash), hash)
}

// Has reports whether a blob with the given hash exists.
func (d *Disk) Has(hash string) (bool, error) {
	if err := safeKey(hash); err != nil {
		return false, err
	}
	_, err := os.Stat(d.path(hash))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Put stores data under hash. It is idempotent (a no-op if the blob exists) and
// writes atomically via a temp file + rename, so a partial write never appears
// as a complete blob.
func (d *Disk) Put(hash string, data []byte) error {
	if err := safeKey(hash); err != nil {
		return err
	}
	if ok, err := d.Has(hash); err != nil || ok {
		return err
	}
	dir := d.shardDir(hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Flush the blob's bytes to the platter before the rename publishes it, so a
	// crash can't leave a truncated file masquerading as a complete blob.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Rename to a non-existent final path: atomic on POSIX, and on Windows too
	// since we never rename over an existing file (Put is a no-op when present).
	if err := os.Rename(tmpName, d.path(hash)); err != nil {
		return err
	}
	// fsync the shard dir so the rename entry itself survives a crash.
	if df, derr := os.Open(dir); derr == nil {
		_ = df.Sync() // ponytail: dir fsync is best-effort; unsupported on Windows
		df.Close()
	}
	return nil
}

// Get returns the blob bytes for hash, or ErrNotFound.
func (d *Disk) Get(hash string) ([]byte, error) {
	if err := safeKey(hash); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(d.path(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Walk calls fn for every blob in the store, passing its hash.
// fn returning a non-nil error stops the walk.
// ponytail: re-hashes every blob (full read); fine for periodic DR checks, sample/mtime-skip if it ever gets too slow on a huge store.
func (d *Disk) Walk(fn func(hash string) error) error {
	return filepath.WalkDir(d.root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		// Skip temp files and anything safeKey rejects (path traversal, etc.).
		if strings.HasPrefix(name, ".") || safeKey(name) != nil {
			return nil
		}
		return fn(name)
	})
}

// Delete removes a blob, returning ErrNotFound if it was absent.
func (d *Disk) Delete(hash string) error {
	if err := safeKey(hash); err != nil {
		return err
	}
	err := os.Remove(d.path(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}
