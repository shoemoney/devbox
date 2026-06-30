// Package daemon runs the live sync loop: for each mount it watches the local
// tree (push on change) and subscribes to hub change events (pull on event),
// funnelling both triggers into a serialized Sync that keeps the mount in
// agreement with the hub.
package daemon

import (
	"context"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shoemoney/devbox/internal/config"
	"github.com/shoemoney/devbox/internal/control"
	"github.com/shoemoney/devbox/internal/hooks"
	"github.com/shoemoney/devbox/internal/ignore"
	"github.com/shoemoney/devbox/internal/secret"
	"github.com/shoemoney/devbox/internal/syncer"
	"github.com/shoemoney/devbox/internal/transport"
	"github.com/shoemoney/devbox/internal/watch"
	"github.com/shoemoney/devbox/pkg/proto"
)

// defaultRescanInterval is the safety-net cadence for re-syncing a mount even
// when no trigger fired. It backstops a watcher that never started (e.g. inotify
// max_user_watches exhausted on a big Pi tree) or an event that slipped through:
// without it, such a mount would stop pushing local edits entirely. PRD risk #1.
// Overridable per machine via settings.sync.rescan_seconds (Daemon.rescanInterval)
// — a huge tree can lengthen it, a latency-sensitive one can shorten it.
const defaultRescanInterval = 60 * time.Second

// Daemon syncs a device's mounts against their hub.
type Daemon struct {
	dir            string
	cfg            config.Daemon
	host           string
	guard          *secret.Guard
	maxBytesPerSec int
	compress       bool          // gzip blob uploads (settings.transfer.compress)
	ignoreDefaults bool          // also ignore common junk dirs (settings.sync.ignore_defaults)
	rescanInterval time.Duration // periodic safety-net cadence (settings.sync.rescan_seconds)
	logf           func(string, ...any)

	paused atomic.Bool // set via control /pause; gates the sync loop

	mu          sync.Mutex
	state       map[string]string       // mountKey -> last-applied snapshot
	nudges      map[string]func()       // mountKey -> trigger a catch-up sync (for Resume)
	mounts      map[string]config.Mount // mountKey -> mount config (for StateSnapshot)
	lastSync    map[string]int64        // mountKey -> unix secs of last successful sync (0 = never)
	resumeTimer *time.Timer             // pending auto-resume from PauseFor; nil if none
}

// New builds a daemon from a loaded config. logf may be nil (defaults to log.Printf).
func New(dir string, cfg config.Daemon, host string, logf func(string, ...any)) (*Daemon, error) {
	settings, err := config.LoadSettings(dir)
	if err != nil {
		return nil, err
	}
	guard, err := secret.New(settings.Secrets.ExtraPatterns)
	if err != nil {
		return nil, err
	}
	state, err := config.LoadState(dir)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = log.Printf
	}
	rescan := defaultRescanInterval
	if settings.Sync.RescanSeconds > 0 {
		rescan = time.Duration(settings.Sync.RescanSeconds) * time.Second
	}
	return &Daemon{
		dir: dir, cfg: cfg, host: host, guard: guard,
		maxBytesPerSec: settings.Transfer.MaxKbps * 1024,
		compress:       settings.Transfer.Compress,
		ignoreDefaults: settings.Sync.IgnoreDefaults,
		rescanInterval: rescan,
		logf:           logf, state: state,
		nudges:   map[string]func(){},
		mounts:   map[string]config.Mount{},
		lastSync: map[string]int64{},
	}, nil
}

// recordSync stamps a mount's last successful sync time (for StateSnapshot / status).
func (d *Daemon) recordSync(m config.Mount) {
	d.mu.Lock()
	d.lastSync[mountKey(m)] = time.Now().Unix()
	d.mu.Unlock()
}

// Run syncs every mount until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Start the control plane before mounts so `devbox status`/`pause` can reach
	// the daemon immediately. A bind failure is non-fatal: sync must not depend on
	// the control socket. Serve stops itself on ctx cancel.
	if _, err := control.Serve(ctx, d.dir, d, d.logf); err != nil {
		d.logf("control socket unavailable (introspection/pause disabled): %v", err)
	}

	var wg sync.WaitGroup
	for _, m := range d.cfg.Mounts {
		wg.Add(1)
		go func(m config.Mount) {
			defer wg.Done()
			d.runMount(ctx, m)
		}(m)
	}
	wg.Wait()
	return nil
}

func mountKey(m config.Mount) string { return m.Share + "\x00" + m.Subpath + "\x00" + m.Local }

func (d *Daemon) getBase(m config.Mount) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state[mountKey(m)]
}

func (d *Daemon) setBase(m config.Mount, base string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state[mountKey(m)] != base {
		d.state[mountKey(m)] = base
		if err := config.SaveState(d.dir, d.state); err != nil {
			d.logf("save state: %v", err)
		}
	}
}

func (d *Daemon) runMount(ctx context.Context, m config.Mount) {
	if m.Pinned {
		// Deployed to a fixed snapshot; never live-advance it. Held until re-mount.
		d.logf("📌 %s pinned to %s — not live-syncing", m.Share, d.getBase(m))
		<-ctx.Done()
		return
	}
	c := transport.New(m.Hub)
	c.SetBearer(d.cfg.Bearer)
	c.SetCompress(d.compress)
	if d.maxBytesPerSec > 0 {
		c.SetRateLimit(d.maxBytesPerSec)
	}

	trigger := make(chan struct{}, 1)
	nudge := func() {
		select {
		case trigger <- struct{}{}:
		default: // a sync is already queued; coalesce
		}
	}
	// Register this mount so the control plane can read its live state and so
	// Resume can kick it into a catch-up sync. Unregister on exit.
	d.register(m, nudge)
	defer d.unregister(m)

	// Local filesystem changes -> sync. If the watcher can't start (e.g. inotify
	// limit), we don't bail: the periodic rescan below keeps the mount syncing.
	if w, err := watch.New(m.Local, 300*time.Millisecond); err != nil {
		d.logf("watch %s: %v — falling back to %s rescans (run: devbox doctor)", m.Local, err, d.rescanInterval)
	} else {
		defer w.Close()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-w.Events():
					nudge()
				}
			}
		}()
	}

	// Hub change events -> sync (reconnect with exponential backoff + jitter).
	go func() {
		backoff := time.Duration(0)
		for ctx.Err() == nil {
			start := time.Now()
			err := c.Events(ctx, m.Share, func(proto.Event) { nudge() })
			if ctx.Err() != nil {
				return
			}
			// A stream that lived a while is a transient drop, not a hard failure:
			// reset so the retry is prompt instead of inheriting a grown delay.
			if time.Since(start) > healthyStream {
				backoff = 0
			}
			backoff = nextBackoff(backoff)
			wait := jittered(backoff)
			d.logf("events %s reconnecting in ~%s: %v", m.Share, wait.Round(time.Millisecond), err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()

	// Periodic rescan: safety net so the mount keeps converging even if the
	// watcher never started (inotify limit) or an event was missed. Stagger the
	// first tick by a random offset so many mounts don't all rebuild manifests in
	// lockstep every interval (a self-synchronized load spike on big trees).
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(rand.Int63n(int64(d.rescanInterval)))):
		}
		t := time.NewTicker(d.rescanInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				nudge()
			}
		}
	}()

	if !d.paused.Load() {
		nudge() // initial sync (skipped while paused)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			// While paused we drain the trigger but don't sync; Resume re-nudges
			// every mount so nothing buffered while paused is lost.
			if d.paused.Load() {
				continue
			}
			d.syncMount(c, m)
		}
	}
}

// register records a mount's nudge and config so Resume can catch it up and the
// control plane can report its live state.
func (d *Daemon) register(m config.Mount, nudge func()) {
	k := mountKey(m)
	d.mu.Lock()
	d.nudges[k] = nudge
	d.mounts[k] = m
	d.mu.Unlock()
}

func (d *Daemon) unregister(m config.Mount) {
	k := mountKey(m)
	d.mu.Lock()
	delete(d.nudges, k)
	delete(d.mounts, k)
	d.mu.Unlock()
}

// Pause stops syncing until Resume. In-flight syncs finish; queued/new triggers
// are drained without effect while paused.
func (d *Daemon) Pause() {
	d.paused.Store(true)
	d.clearResumeTimer() // an explicit pause overrides any pending auto-resume
	d.logf("⏸️  paused — syncing suspended (devbox resume to continue)")
}

// PauseFor pauses syncing and auto-resumes after dur (devbox pause --for). A new
// pause replaces any pending auto-resume; an explicit Resume cancels it. A
// non-positive dur is an indefinite pause, identical to Pause.
func (d *Daemon) PauseFor(dur time.Duration) {
	d.Pause() // clears any prior timer (fully unlocks before we re-lock below)
	if dur <= 0 {
		return
	}
	d.mu.Lock()
	d.resumeTimer = time.AfterFunc(dur, d.Resume)
	d.mu.Unlock()
	d.logf("⏱️  auto-resume scheduled in %s", dur)
}

// clearResumeTimer stops and drops any pending PauseFor auto-resume.
func (d *Daemon) clearResumeTimer() {
	d.mu.Lock()
	if d.resumeTimer != nil {
		d.resumeTimer.Stop()
		d.resumeTimer = nil
	}
	d.mu.Unlock()
}

// Resume clears the pause and nudges every registered mount into an immediate
// catch-up sync, so edits made while paused converge right away.
func (d *Daemon) Resume() {
	d.paused.Store(false)
	d.mu.Lock()
	if d.resumeTimer != nil { // cancel any pending auto-resume from PauseFor
		d.resumeTimer.Stop()
		d.resumeTimer = nil
	}
	nudges := make([]func(), 0, len(d.nudges))
	for _, n := range d.nudges {
		nudges = append(nudges, n)
	}
	d.mu.Unlock()
	for _, n := range nudges {
		n()
	}
	d.logf("▶️  resumed — catching up %d mount(s)", len(nudges))
}

// StateSnapshot returns the running daemon's live per-mount + paused view for
// the control plane's GET /state.
func (d *Daemon) StateSnapshot() control.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := control.State{Paused: d.paused.Load()}
	for k, m := range d.mounts {
		st.Mounts = append(st.Mounts, control.MountState{
			Share:        m.Share,
			Subpath:      m.Subpath,
			Local:        m.Local,
			ReadOnly:     m.ReadOnly,
			Pinned:       m.Pinned,
			BaseSnapshot: d.state[k],
			LastSyncUnix: d.lastSync[k],
		})
	}
	return st
}

func (d *Daemon) syncMount(c *transport.Client, m config.Mount) {
	excludes := m.Exclude
	if d.ignoreDefaults {
		excludes = append(append([]string{}, m.Exclude...), ignore.Defaults...)
	}
	ig, err := syncer.LoadIgnoreWith(m.Local, excludes)
	if err != nil {
		d.logf("ignore %s: %v", m.Local, err)
		return
	}
	base := d.getBase(m)
	now := time.Now().UnixNano() // nanoseconds so conflict-copy names don't collide
	hk := hooks.New(m.Local, m.Share, d.host, m.Hub)

	if m.ReadOnly {
		pr, err := syncer.Pull(c, m.Local, m.Share, m.Subpath, base, d.host, now, ig, d.guard, hk)
		if err != nil {
			d.logf("pull %s: %v", m.Share, err)
			return
		}
		d.setBase(m, pr.Base)
		d.recordSync(m)
		d.report(m, pr)
		return
	}

	newBase, pr, err := syncer.Sync(c, m.Local, m.Share, m.Subpath, base, d.host, now, ig, d.guard, hk)
	if err != nil {
		d.logf("sync %s: %v", m.Share, err)
		return
	}
	d.setBase(m, newBase)
	d.recordSync(m)
	d.report(m, pr)
}

func (d *Daemon) report(m config.Mount, pr syncer.PullResult) {
	if len(pr.Written)+len(pr.Deleted)+len(pr.Conflicts)+len(pr.Skipped) == 0 {
		return
	}
	d.logf("📥 %s: %d written, %d deleted, %d conflicts, %d skipped",
		m.Share, len(pr.Written), len(pr.Deleted), len(pr.Conflicts), len(pr.Skipped))
	for _, cf := range pr.Conflicts {
		d.logf("   💥 conflict copy: %s", cf)
	}
	for _, sk := range pr.Skipped {
		d.logf("   ⚠️  skipped (filesystem clash): %s", sk)
	}
}
