package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// --- pidfile: lets `devbox stop` find a running `devbox start` ---
//
// The file holds "<pid> <starttoken>". The start token is a per-process-start
// identity (the OS process start time) that changes when a PID is recycled.
// After a crash the OS can hand our old PID to an unrelated process; matching
// the token ensures we only ever signal — or refuse to start over — OUR daemon,
// never a stranger that happens to wear the same PID. When the token can't be
// determined (non-Linux, or /proc unreadable) we fall back to bare-PID liveness,
// which is no worse than before.

func pidPath(dir string) string { return filepath.Join(dir, "daemon.pid") }

// writePid atomically claims the pidfile via O_EXCL so two `devbox start`
// invocations can't both think they're the only daemon. If the file already
// exists it arbitrates: a live pid means refuse; a stale pid (crashed daemon,
// or a recycled pid that isn't us) is removed and the claim retried.
func writePid(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for {
		f, err := os.OpenFile(pidPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(pidfileContents())
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			return werr
		}
		if !os.IsExist(err) {
			return err
		}
		if pid, ok := runningPid(dir); ok {
			return fmt.Errorf("daemon already running (pid %d) — run: devbox stop", pid)
		}
		if rerr := os.Remove(pidPath(dir)); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		// Stale pidfile removed; loop retries the exclusive create.
	}
}

// pidfileContents is the line we record for our own process: "<pid>" or, when
// the start token is available, "<pid> <starttoken>".
func pidfileContents() string {
	pid := os.Getpid()
	if tok, ok := processStartToken(pid); ok {
		return strconv.Itoa(pid) + " " + tok
	}
	return strconv.Itoa(pid)
}

func removePid(dir string) { _ = os.Remove(pidPath(dir)) }

// runningPid reports the pid in the pidfile, and whether that process is alive
// AND is our daemon. A stale pidfile (process gone, or the pid recycled by an
// unrelated process) returns ok=false.
func runningPid(dir string) (int, bool) {
	b, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return 0, false
	}
	pid, tok, ok := parsePidfile(string(b))
	if !ok {
		return 0, false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	// On unix signal 0 tests liveness without delivering a signal.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	// If both sides have a start token, they must match: a mismatch means the
	// pid was recycled and now belongs to some other process — not our daemon.
	if tok != "" {
		if cur, vok := processStartToken(pid); vok && cur != tok {
			return 0, false
		}
	}
	return pid, true
}

// parsePidfile splits "<pid>" or "<pid> <starttoken>" into its fields.
func parsePidfile(s string) (pid int, token string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	if len(fields) > 1 {
		token = fields[1]
	}
	return pid, token, true
}

// processStartToken returns a per-process-start identity for pid that is stable
// for the life of that process but changes when the OS recycles the pid. ok is
// false when the platform can't supply one, in which case callers fall back to
// bare-pid liveness.
//
// Linux: field 22 (starttime, in clock ticks since boot) of /proc/<pid>/stat.
// Other OSes: unverifiable.
func processStartToken(pid int) (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", false
	}
	// The comm field (field 2) is parenthesized and may itself contain spaces
	// or ')', so split on the LAST ')' before counting space-separated fields.
	line := string(b)
	close := strings.LastIndexByte(line, ')')
	if close < 0 || close+2 > len(line) {
		return "", false
	}
	rest := strings.Fields(line[close+2:]) // fields starting at #3 (state)
	// starttime is field 22; we've dropped fields 1 and 2, so it's index 19.
	const starttimeIdx = 19
	if len(rest) <= starttimeIdx {
		return "", false
	}
	tok := rest[starttimeIdx]
	if tok == "" {
		return "", false
	}
	return tok, true
}
