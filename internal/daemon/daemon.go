// Package daemon runs the live sync loop: for each mount it watches the local
// tree (push on change) and subscribes to hub change events (pull on event),
// funnelling both triggers into a serialized Sync that keeps the mount in
// agreement with the hub.
package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/config"
	"git.shoemoney.ai/shoemoney/devbox/internal/hooks"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/syncer"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
	"git.shoemoney.ai/shoemoney/devbox/internal/watch"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// Daemon syncs a device's mounts against their hub.
type Daemon struct {
	dir            string
	cfg            config.Daemon
	host           string
	guard          *secret.Guard
	maxBytesPerSec int
	logf           func(string, ...any)

	mu    sync.Mutex
	state map[string]string // mountKey -> last-applied snapshot
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
	return &Daemon{
		dir: dir, cfg: cfg, host: host, guard: guard,
		maxBytesPerSec: settings.Transfer.MaxKbps * 1024,
		logf:           logf, state: state,
	}, nil
}

// Run syncs every mount until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
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

	// Local filesystem changes -> sync.
	if w, err := watch.New(m.Local, 300*time.Millisecond); err != nil {
		d.logf("watch %s: %v", m.Local, err)
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
			if time.Since(start) > backoffMax {
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

	nudge() // initial sync
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			d.syncMount(c, m)
		}
	}
}

func (d *Daemon) syncMount(c *transport.Client, m config.Mount) {
	ig, err := syncer.LoadIgnore(m.Local)
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
		d.report(m, pr)
		return
	}

	newBase, pr, err := syncer.Sync(c, m.Local, m.Share, m.Subpath, base, d.host, now, ig, d.guard, hk)
	if err != nil {
		d.logf("sync %s: %v", m.Share, err)
		return
	}
	d.setBase(m, newBase)
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
