package daemon

import (
	"math/rand"
	"time"
)

// Reconnect backoff bounds: start at 1s, double to a 30s ceiling. A stream that
// stayed up at least healthyStream is treated as a transient drop (reset to base),
// not a hard failure — so a hub that recycles SSE connections every ~30s doesn't
// leave the daemon stuck at the ceiling with a 30s event lag.
const (
	backoffBase   = 1 * time.Second
	backoffMax    = 30 * time.Second
	healthyStream = 2 * time.Minute
)

// nextBackoff returns the next clean (un-jittered) backoff: base, then doubling
// up to backoffMax. Pass the previous clean value; pass <=0 to (re)start at base.
func nextBackoff(prev time.Duration) time.Duration {
	switch {
	case prev <= 0:
		return backoffBase
	case prev*2 >= backoffMax:
		return backoffMax
	default:
		return prev * 2
	}
}

// jittered spreads a backoff by ±25% so a fleet that all lost the hub at once
// doesn't reconnect in lockstep (thundering herd). Returns 0 unchanged.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration(rand.Int63n(int64(d/2))) - d/4
}
