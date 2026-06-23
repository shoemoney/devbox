package transport

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// rlBurst caps a single throttled Read. It equals one max chunk so WaitN(n) is
// always called with n <= burst (rate.Limiter.WaitN errors if n > burst).
const rlBurst = 64 * 1024

// SetRateLimit caps blob transfer (up and down) to bytesPerSec. 0 = unlimited.
func (c *Client) SetRateLimit(bytesPerSec int) {
	if bytesPerSec <= 0 {
		c.limiter = nil
		return
	}
	c.limiter = rate.NewLimiter(rate.Limit(bytesPerSec), rlBurst)
}

// limit wraps r so reads are paced by the client's limiter (no-op if unset).
func (c *Client) limit(r io.Reader) io.Reader {
	if c.limiter == nil {
		return r
	}
	return &rlReader{r: r, lim: c.limiter}
}

type rlReader struct {
	r   io.Reader
	lim *rate.Limiter
}

func (rr *rlReader) Read(p []byte) (int, error) {
	if len(p) > rlBurst {
		p = p[:rlBurst]
	}
	n, err := rr.r.Read(p)
	if n > 0 {
		_ = rr.lim.WaitN(context.Background(), n)
	}
	return n, err
}
