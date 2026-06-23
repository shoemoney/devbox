package chunk

import (
	"bytes"
	"math/rand"
	"testing"
)

// seededBlob returns a deterministic pseudo-random blob of n bytes.
func seededBlob(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	// rand.Read on a seeded source is deterministic.
	r.Read(b)
	return b
}

func mustSplit(t *testing.T, data []byte) []Chunk {
	t.Helper()
	c, err := Split(data)
	if err != nil {
		t.Fatalf("Split error: %v", err)
	}
	return c
}

func TestHashKnownVector(t *testing.T) {
	// BLAKE3-256 of the empty input.
	const want = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"
	if got := Hash([]byte("")); got != want {
		t.Fatalf("Hash(\"\") = %q, want %q", got, want)
	}
}

func TestHashDeterministic(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", []byte("")},
		{"short", []byte("hello world")},
		{"binary", []byte{0x00, 0xff, 0x10, 0x80, 0x7f}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if Hash(tt.in) != Hash(tt.in) {
				t.Fatalf("Hash not deterministic for %q", tt.name)
			}
		})
	}
}

func TestSplitDeterministic(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"tiny", []byte("a few bytes")},
		{"sub-min", seededBlob(1, MinSize-1)},
		{"medium", seededBlob(2, 256*1024)},
		{"large", seededBlob(3, 1<<20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := mustSplit(t, tt.data)
			b := mustSplit(t, tt.data)
			if len(a) != len(b) {
				t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
			}
			for i := range a {
				if a[i] != b[i] {
					t.Fatalf("chunk %d differs: %+v vs %+v", i, a[i], b[i])
				}
			}
		})
	}
}

func TestSplitReassembly(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"tiny", []byte("hello")},
		{"exactly-min", seededBlob(10, MinSize)},
		{"sub-min", seededBlob(11, MinSize-1)},
		{"medium", seededBlob(12, 100*1024)},
		{"large", seededBlob(13, 1<<20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := mustSplit(t, tt.data)

			// Offsets contiguous starting at 0; sizes sum to len(data).
			var want int64
			var total int64
			var reassembled bytes.Buffer
			for i, c := range chunks {
				if c.Offset != want {
					t.Fatalf("chunk %d offset = %d, want %d", i, c.Offset, want)
				}
				if c.Size <= 0 {
					t.Fatalf("chunk %d has non-positive size %d", i, c.Size)
				}
				reassembled.Write(tt.data[c.Offset : c.Offset+c.Size])
				// Hash recorded must match the chunk's actual bytes.
				if got := Hash(tt.data[c.Offset : c.Offset+c.Size]); got != c.Hash {
					t.Fatalf("chunk %d hash mismatch: stored %s, recomputed %s", i, c.Hash, got)
				}
				want += c.Size
				total += c.Size
			}
			if total != int64(len(tt.data)) {
				t.Fatalf("sizes sum to %d, want %d", total, len(tt.data))
			}
			if !bytes.Equal(reassembled.Bytes(), tt.data) {
				t.Fatalf("reassembled data != original (len %d vs %d)", reassembled.Len(), len(tt.data))
			}
		})
	}
}

func TestSplitEmpty(t *testing.T) {
	// Chosen behavior: empty input => zero chunks.
	if got := mustSplit(t, nil); len(got) != 0 {
		t.Fatalf("Split(nil) = %d chunks, want 0", len(got))
	}
	if got := mustSplit(t, []byte{}); len(got) != 0 {
		t.Fatalf("Split([]byte{}) = %d chunks, want 0", len(got))
	}
}

func TestSplitSubMinSingleChunk(t *testing.T) {
	data := seededBlob(99, MinSize-1)
	chunks := mustSplit(t, data)
	if len(chunks) != 1 {
		t.Fatalf("sub-min input produced %d chunks, want 1", len(chunks))
	}
	if chunks[0].Offset != 0 || chunks[0].Size != int64(len(data)) {
		t.Fatalf("single chunk = {off %d, size %d}, want {0, %d}", chunks[0].Offset, chunks[0].Size, len(data))
	}
}

func TestSplitDedupLocalizesEdit(t *testing.T) {
	const size = 1 << 20 // 1 MiB
	orig := seededBlob(42, size)

	// Copy and mutate ~16 bytes in the middle.
	mutated := append([]byte(nil), orig...)
	mid := size / 2
	for i := 0; i < 16; i++ {
		mutated[mid+i] ^= 0xAB // flip bits; deterministic mutation
	}

	origChunks := mustSplit(t, orig)
	mutChunks := mustSplit(t, mutated)

	if len(origChunks) < 4 {
		t.Fatalf("expected several chunks for a 1 MiB blob, got %d", len(origChunks))
	}

	origSet := make(map[string]struct{}, len(origChunks))
	for _, c := range origChunks {
		origSet[c.Hash] = struct{}{}
	}

	shared := 0
	for _, c := range mutChunks {
		if _, ok := origSet[c.Hash]; ok {
			shared++
		}
	}

	ratio := float64(shared) / float64(len(mutChunks))
	if ratio <= 0.5 {
		t.Fatalf("dedup ratio %.2f (%d/%d) <= 0.50; content-defined boundaries should localize a 16-byte edit",
			ratio, shared, len(mutChunks))
	}
	t.Logf("dedup: %d/%d mutated chunks (%.1f%%) shared with original", shared, len(mutChunks), ratio*100)
}
