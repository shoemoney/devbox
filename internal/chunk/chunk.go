// Package chunk implements content-defined chunking (FastCDC) plus BLAKE3-256
// hashing. Together these form the dedup core: identical content produces
// identical chunk boundaries and hashes regardless of where it appears, so
// unchanged regions of a mutated file dedup against the original.
package chunk

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	fastcdc "github.com/jotfs/fastcdc-go"
	"github.com/zeebo/blake3"
)

// ponytail: jotfs/fastcdc-go mutates a package-global table inside NewChunker
// (table[i] ^= Seed) on every call, so concurrent Split races even with Seed 0.
// Serialize chunking per process. Upgrade path: vendor a FastCDC with a
// per-chunker table if chunking throughput ever bottlenecks a single daemon.
var splitMu sync.Mutex

// Chunk size targets. FastCDC keeps chunk lengths within [min, max] and aims
// for the average via content-defined boundaries.
const (
	MinSize = 4 * 1024  // 4 KiB
	AvgSize = 16 * 1024 // 16 KiB
	MaxSize = 64 * 1024 // 64 KiB
)

// Chunk describes a single content-defined chunk of an input buffer.
// Hash is the lowercase hex BLAKE3-256 digest of the chunk's bytes.
type Chunk struct {
	Hash   string
	Offset int64
	Size   int64
}

// Split divides data into content-defined chunks using FastCDC and returns
// them in order. The returned chunks are contiguous: chunk[0].Offset == 0,
// each chunk starts where the previous ended, and the sizes sum to len(data).
//
// Inputs smaller than MinSize yield a single chunk. Empty input yields zero
// chunks. A genuine read error is returned (never silently truncated) — this
// matters once Split streams from an io.Reader, where a mid-input error would
// otherwise produce a short, valid-looking chunk list with the wrong hash.
//
// ponytail: Split is in-memory — it takes the whole []byte and the underlying
// chunker reads from a bytes.Reader over it. Streaming large inputs from an
// io.Reader without buffering the entire payload is the upgrade path.
func Split(data []byte) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, nil
	}
	splitMu.Lock()
	defer splitMu.Unlock()

	chunker, err := fastcdc.NewChunker(bytes.NewReader(data), fastcdc.Options{
		MinSize:     MinSize,
		AverageSize: AvgSize,
		MaxSize:     MaxSize,
	})
	if err != nil {
		return nil, err
	}

	var chunks []Chunk
	for {
		c, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, Chunk{
			Hash:   Hash(c.Data),
			Offset: int64(c.Offset),
			Size:   int64(c.Length),
		})
	}
	return chunks, nil
}

// Hash returns the BLAKE3-256 digest of b as a lowercase hex string.
func Hash(b []byte) string {
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}
