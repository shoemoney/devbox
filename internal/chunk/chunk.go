// Package chunk implements content-defined chunking (FastCDC) plus BLAKE3-256
// hashing. Together these form the dedup core: identical content produces
// identical chunk boundaries and hashes regardless of where it appears, so
// unchanged regions of a mutated file dedup against the original.
package chunk

import (
	"bytes"
	"encoding/hex"

	fastcdc "github.com/jotfs/fastcdc-go"
	"github.com/zeebo/blake3"
)

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
// chunks.
//
// ponytail: Split is in-memory — it takes the whole []byte and the underlying
// chunker reads from a bytes.Reader over it. Streaming large inputs from an
// io.Reader without buffering the entire payload is the upgrade path.
func Split(data []byte) []Chunk {
	if len(data) == 0 {
		return nil
	}

	chunker, err := fastcdc.NewChunker(bytes.NewReader(data), fastcdc.Options{
		MinSize:     MinSize,
		AverageSize: AvgSize,
		MaxSize:     MaxSize,
	})
	if err != nil {
		// Options are compile-time constants that satisfy the library's
		// validation, so construction cannot fail in practice. Fall back to
		// treating the whole input as one chunk rather than panicking.
		return []Chunk{{Hash: Hash(data), Offset: 0, Size: int64(len(data))}}
	}

	var chunks []Chunk
	for {
		c, err := chunker.Next()
		if err != nil {
			// io.EOF (or any error) ends iteration; on a bytes.Reader the only
			// terminal condition is EOF after the final chunk.
			break
		}
		chunks = append(chunks, Chunk{
			Hash:   Hash(c.Data),
			Offset: int64(c.Offset),
			Size:   int64(c.Length),
		})
	}
	return chunks
}

// Hash returns the BLAKE3-256 digest of b as a lowercase hex string.
func Hash(b []byte) string {
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}
