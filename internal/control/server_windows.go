//go:build windows

package control

import (
	"context"
	"errors"
)

// ponytail: Windows gets a stub, not a real control plane. The fleet runs
// darwin/linux; a Windows port should serve the same HTTP/1.1 API over a named
// pipe (`\\.\pipe\devbox-control`) via Microsoft's go-winio — swap the
// net.Listen("unix", …) in server.go for winio.ListenPipe and the client's
// net.Dial for winio.DialPipe. Until a Windows user exists, we don't build it.

// Daemon is the slice of the running daemon the control server would steer.
type Daemon interface {
	StateSnapshot() State
	Pause()
	Resume()
}

// Server is a no-op on Windows.
type Server struct{}

// Serve reports that the control socket isn't supported on Windows yet. The
// daemon warns and continues; sync is unaffected.
func Serve(_ context.Context, _ string, _ Daemon, _ func(string, ...any)) (*Server, error) {
	return nil, errors.New("control socket not supported on windows yet")
}

// Publish is a no-op on Windows.
func (s *Server) Publish(_, _ string) {}
