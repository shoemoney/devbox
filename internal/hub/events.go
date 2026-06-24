package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/shoemoney/devbox/pkg/proto"
)

// broker fans hub change events out to connected SSE subscribers. A slow
// subscriber's events are dropped rather than blocking the push path — the
// device re-syncs via Head() regardless, so a missed nudge is harmless.
type broker struct {
	mu   sync.Mutex
	subs map[chan proto.Event]string // channel -> share filter ("" = all shares)
}

func newBroker() *broker { return &broker{subs: map[chan proto.Event]string{}} }

func (b *broker) sub(share string) chan proto.Event {
	ch := make(chan proto.Event, 16)
	b.mu.Lock()
	b.subs[ch] = share
	b.mu.Unlock()
	return ch
}

func (b *broker) unsub(ch chan proto.Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// publish delivers ev to every subscriber whose filter matches. Held under the
// same lock as unsub, so a channel is never sent-to after it is closed.
func (b *broker) publish(ev proto.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, filter := range b.subs {
		if filter != "" && filter != ev.Share {
			continue
		}
		select {
		case ch <- ev:
		default: // subscriber is slow; drop (it will re-sync via Head())
		}
	}
}

// handleEvents streams change events to a device as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, _ string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.broker.sub(r.URL.Query().Get("share"))
	defer s.broker.unsub(ch)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n") // comment line keeps idle proxies from closing us
			flusher.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}
