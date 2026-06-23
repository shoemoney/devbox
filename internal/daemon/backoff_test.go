package daemon

import (
	"testing"
	"time"
)

func TestNextBackoffDoublesAndCaps(t *testing.T) {
	// Starts at base, doubles, then saturates at the ceiling.
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	got := time.Duration(0)
	for i, w := range want {
		got = nextBackoff(got)
		if got != w {
			t.Fatalf("step %d: nextBackoff = %s, want %s", i, got, w)
		}
	}
	// A non-positive input restarts at base (used to reset after a healthy stream).
	if d := nextBackoff(0); d != backoffBase {
		t.Fatalf("reset: nextBackoff(0) = %s, want %s", d, backoffBase)
	}
}

func TestJitteredStaysWithinBand(t *testing.T) {
	const d = 8 * time.Second
	lo, hi := d-d/4, d+d/4 // ±25%
	for range 1000 {
		j := jittered(d)
		if j < lo || j >= hi {
			t.Fatalf("jittered(%s) = %s, want in [%s,%s)", d, j, lo, hi)
		}
	}
	if jittered(0) != 0 {
		t.Fatal("jittered(0) should be 0")
	}
}
