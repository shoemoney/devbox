//go:build !windows

package control

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"time"
)

// Server hosts the control API on a Unix socket. The zero value is unusable;
// build one with Serve.
type Server struct {
	d    Daemon
	ln   net.Listener
	srv  *http.Server
	sock string
}

// Serve binds the control socket under dir (mode 0600, stale socket removed
// first) and serves the control API until ctx is cancelled, then cleans up. It
// returns once the listener is bound; the serving runs in the background. A bind
// failure is returned so the caller can warn-and-continue — sync must not depend
// on the control plane.
//
// ponytail: GET /events (an SSE activity stream) is intentionally NOT served — it
// only feeds the M11 TUI, which doesn't exist yet. Add the route + a fan-out
// broker when a real consumer lands; today /state + /pause + /resume cover every
// caller (devbox status/pause/resume).
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

	s := &Server{d: d, ln: ln, sock: sock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", s.handleState)
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

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("for"); v != "" {
		dur, err := time.ParseDuration(v)
		if err != nil || dur <= 0 {
			http.Error(w, "bad for= duration", http.StatusBadRequest)
			return
		}
		s.d.PauseFor(dur)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.d.Pause()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.d.Resume()
	w.WriteHeader(http.StatusOK)
}
