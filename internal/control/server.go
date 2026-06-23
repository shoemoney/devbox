//go:build !windows

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Daemon is the slice of the running daemon the control server steers. The
// daemon package implements it; kept here so the server depends on a behaviour,
// not on the daemon package (no import cycle).
type Daemon interface {
	StateSnapshot() State // live per-mount + paused view for GET /state
	Pause()               // stop syncing until resumed
	Resume()              // clear pause + catch up all mounts
}

// Server hosts the control API on a Unix socket. The zero value is unusable;
// build one with Serve.
type Server struct {
	d      Daemon
	broker *broker
	ln     net.Listener
	srv    *http.Server
	sock   string
}

// Serve binds the control socket under dir (mode 0600, stale socket removed
// first) and serves the control API until ctx is cancelled, then cleans up. It
// returns once the listener is bound; the serving runs in the background. A bind
// failure is returned so the caller can warn-and-continue — sync must not depend
// on the control plane.
func Serve(ctx context.Context, dir string, d Daemon, logf func(string, ...any)) (*Server, error) {
	sock := SockPath(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A stale socket from a crashed daemon would make Listen fail with
	// "address already in use"; remove it first (it's ours by location).
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	// Tighten to owner-only: the socket is the daemon's authority boundary.
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		os.Remove(sock)
		return nil, err
	}

	s := &Server{d: d, broker: newBroker(), ln: ln, sock: sock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /pause", s.handlePause)
	mux.HandleFunc("POST /resume", s.handleResume)
	s.srv = &http.Server{Handler: mux}

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed && logf != nil {
			logf("control server: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		s.close()
	}()
	return s, nil
}

// Publish reports that a mount finished a sync; the server fans it out to any
// GET /events subscribers. Safe to call from the sync goroutines.
func (s *Server) Publish(share, detail string) {
	if s == nil {
		return
	}
	s.broker.publish(activity{Share: share, Detail: detail, At: time.Now().Unix()})
}

func (s *Server) close() {
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(shutCtx)
	_ = os.Remove(s.sock)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.d.StateSnapshot())
}

func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	s.d.Pause()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.d.Resume()
	w.WriteHeader(http.StatusOK)
}

// handleEvents streams sync activity as Server-Sent Events, mirroring the hub's
// SSE wire style (`data: <json>\n\n`, periodic `: ping` comments to keep idle
// proxies open). ponytail: copied, not imported — internal/hub is off-limits.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := s.broker.sub()
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
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// activity is one line of sync activity emitted on GET /events.
type activity struct {
	Share  string `json:"share"`
	Detail string `json:"detail"`
	At     int64  `json:"at"` // unix seconds
}

// broker fans sync activity out to connected SSE subscribers. A slow subscriber
// is dropped rather than blocking the sync path — same trade-off as the hub's
// broker. ponytail: pattern copied from internal/hub/events.go.
type broker struct {
	mu   sync.Mutex
	subs map[chan activity]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan activity]struct{}{}} }

func (b *broker) sub() chan activity {
	ch := make(chan activity, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsub(ch chan activity) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *broker) publish(ev activity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber; drop
		}
	}
}
