package daemon

import (
	"math/rand"
	"time"
)

// Reconnect backoff bounds: start at 1s, double to a 30s ceiling. A stream that
// stayed up at least healthyStream is treated as a transient drop (reset to base),
// not a hard failure — so a hub/proxy that recycles SSE connections periodically
// doesn't leave the daemon stuck at the ceiling with a 30s event lag.
//
// healthyStream must sit BELOW the recycle interval (proxies commonly cap idle
// SSE around 30-60s): 2 minutes was 4x too large to ever fire, so a recycled-but-
// healthy stream still ratcheted backoff up to the ceiling.
const (
	backoffBase   = 1 * time.Second
	backoffMax    = 30 * time.Second
	healthyStream = 20 * time.Second
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
	half := int64(d / 2)
	if half <= 0 {
		return d // sub-2ns: no room to jitter, and rand.Int63n(0) would panic
	}
	return d + time.Duration(rand.Int63n(half)) - d/4
}
