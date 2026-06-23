package transport

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// TestRateLimitThrottles checks the limiter can't transfer faster than its cap
// (a reliable lower bound — it may be slower, never faster).
func TestRateLimitThrottles(t *testing.T) {
	c := New("http://x")
	c.SetRateLimit(100 * 1024) // 100 KiB/s

	const n = 250 * 1024 // 250 KiB -> ~2.5s at the cap
	start := time.Now()
	got, err := io.Copy(io.Discard, c.limit(bytes.NewReader(make([]byte, n))))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("copied %d bytes, want %d", got, n)
	}
	// Allow generous slack but ensure it actually throttled (burst lets the first
	// 64 KiB through free, so expect at least ~1.5s for the remaining ~186 KiB).
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("transfer took %v, expected the 100 KiB/s cap to slow it down", elapsed)
	}
}

func TestRateLimitUnlimitedByDefault(t *testing.T) {
	c := New("http://x")
	if c.limiter != nil {
		t.Fatal("new client should be unlimited")
	}
	c.SetRateLimit(0)
	if c.limiter != nil {
		t.Fatal("SetRateLimit(0) should clear the limiter")
	}
	// limit() is a pass-through when unset.
	r := bytes.NewReader([]byte("hi"))
	if c.limit(r) != r {
		t.Fatal("limit() should return the reader unchanged when unlimited")
	}
}
