// Package dashboard is the hub's OPTIONAL live dashboard: an embedded web UI
// plus a JSON state snapshot (/api/state) and a Server-Sent-Events flow stream
// (/api/events) that animates devices pushing/joining in real time. It is read-
// only and served on its own address (localhost by default) so it never widens
// the hub's API surface; a nil *Dashboard means "disabled" and is safe to Emit to.
package dashboard

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shoemoney/devbox/internal/hub/meta"
)

//go:embed index.html
var indexHTML []byte

const onlineWindow = 2 * time.Minute

// Event is one live activity event streamed to the dashboard UI. It is NOT wire-
// versioned (UI feed only), so it can change freely without breaking clients.
type Event struct {
	TS         int64  `json:"ts"`   // unix millis
	Type       string `json:"type"` // "join" | "push" | "pull" | "conflict" | "gc"
	Device     string `json:"device"`
	DeviceName string `json:"device_name"`
	Share      string `json:"share,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
	Pruned     int    `json:"pruned,omitempty"` // gc: snapshots pruned
	Snapshot   string `json:"snapshot,omitempty"`
	NewHead    bool   `json:"new_head,omitempty"`
}

// histWindow is how many one-minute activity buckets the dashboard retains so the
// sparkline survives a page reload (it was previously computed from the live
// stream only, so it went blank on refresh).
const histWindow = 60

// histBucket is one minute of aggregated activity for the sparkline.
type histBucket struct {
	TS        int64 `json:"ts"` // unix sec, minute-aligned
	Bytes     int64 `json:"bytes"`
	Pushes    int   `json:"pushes"`
	Pulls     int   `json:"pulls"`
	Conflicts int   `json:"conflicts"`
	GCs       int   `json:"gcs"`
}

// Dashboard holds the live state broker and the read handles into the metadata DB.
type Dashboard struct {
	db      *meta.DB
	version string
	started time.Time
	broker  *broker
	token   string // optional shared secret; "" = unauthenticated (loopback default)

	mu       sync.Mutex
	lastSeen map[string]int64 // deviceID -> unix seconds of its last activity
	hist     []histBucket     // rolling per-minute activity, oldest→newest (≤ histWindow)
}

// New builds a dashboard over the hub's metadata DB.
func New(db *meta.DB, version string) *Dashboard {
	return &Dashboard{db: db, version: version, started: time.Now(), broker: newBroker(), lastSeen: map[string]int64{}}
}

// SetToken requires callers to present the token (so a non-loopback dashboard
// isn't world-readable). Empty token leaves it open. Must be called before
// Handler.
func (d *Dashboard) SetToken(tok string) { d.token = tok }

// Emit records device activity and fans the event out to connected dashboards.
// Safe on a nil receiver (dashboard disabled) so hub handlers call it freely.
func (d *Dashboard) Emit(ev Event) {
	if d == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	now := time.Now().Unix()
	d.mu.Lock()
	if ev.Device != "" {
		d.lastSeen[ev.Device] = now
	}
	d.recordLocked(ev, now)
	d.mu.Unlock()
	d.broker.publish(ev)
}

// recordLocked folds an event into the current minute's history bucket. Caller
// holds d.mu. Buckets older than histWindow minutes are dropped.
func (d *Dashboard) recordLocked(ev Event, now int64) {
	min := now - now%60
	if n := len(d.hist); n == 0 || d.hist[n-1].TS != min {
		d.hist = append(d.hist, histBucket{TS: min})
		if len(d.hist) > histWindow {
			d.hist = d.hist[len(d.hist)-histWindow:]
		}
	}
	b := &d.hist[len(d.hist)-1]
	switch ev.Type {
	case "push":
		b.Pushes++
		b.Bytes += ev.Bytes
	case "pull":
		b.Pulls++
	case "conflict":
		b.Conflicts++
	case "gc":
		b.GCs++
	}
}

// history returns a copy of the rolling activity window for /api/state.
func (d *Dashboard) history() []histBucket {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]histBucket, len(d.hist))
	copy(out, d.hist)
	return out
}

func (d *Dashboard) online(deviceID string, now int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ts, ok := d.lastSeen[deviceID]
	return ok && now-ts <= int64(onlineWindow/time.Second)
}

// Handler serves the UI (/), the state snapshot (/api/state) and the live flow
// stream (/api/events).
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", d.handleState)
	mux.HandleFunc("GET /api/events", d.handleEvents)
	return d.withAuth(mux)
}

const dashCookie = "devbox_dash"

// withAuth gates the dashboard behind d.token when one is set. The token may be
// presented as `?token=`, an `Authorization: Bearer` header (for scripts), or a
// cookie. A `?token=` on any request pins an HttpOnly cookie so the embedded
// page's fetch()/EventSource calls — which can't set headers — authenticate
// automatically; this keeps index.html untouched. Open (no token) → pass-through.
func (d *Dashboard) withAuth(next http.Handler) http.Handler {
	if d.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("token"); q != "" && tokenEqual(q, d.token) {
			http.SetCookie(w, &http.Cookie{Name: dashCookie, Value: d.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			if r.URL.Path == "/" { // drop the secret from the address bar / history
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		const bp = "Bearer "
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bp) && tokenEqual(h[len(bp):], d.token) {
			next.ServeHTTP(w, r)
			return
		}
		if ck, err := r.Cookie(dashCookie); err == nil && tokenEqual(ck.Value, d.token) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="devbox dashboard"`)
		http.Error(w, "unauthorized — open the dashboard with ?token=<dashboard token>", http.StatusUnauthorized)
	})
}

// tokenEqual compares in constant time so a wrong guess leaks no length/prefix timing.
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type stateResp struct {
	Hub     hubInfo      `json:"hub"`
	Totals  totalsInfo   `json:"totals"`
	Devices []deviceJSON `json:"devices"`
	Shares  []shareJSON  `json:"shares"`
	History []histBucket `json:"history"` // rolling per-minute activity for the sparkline
}
type hubInfo struct {
	Version string `json:"version"`
	UptimeS int64  `json:"uptime_s"`
}
type totalsInfo struct {
	Devices   int64 `json:"devices"`
	Online    int64 `json:"online"`
	Shares    int64 `json:"shares"`
	Snapshots int64 `json:"snapshots"`
	Chunks    int64 `json:"chunks"`
	Bytes     int64 `json:"bytes"`
}

type deviceJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	LastSeen  int64  `json:"last_seen"`
	Online    bool   `json:"online"`
	Principal string `json:"principal"`
	Revoked   bool   `json:"revoked"`
}
type shareJSON struct {
	Name      string `json:"name"`
	Head      string `json:"head"`
	Snapshots int    `json:"snapshots"`
	Members   int    `json:"members"`
	ACLMode   string `json:"acl_mode"`
	UpdatedAt int64  `json:"updated_at"`
}

func (d *Dashboard) handleState(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	devs, err := d.db.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var online int64
	devices := make([]deviceJSON, 0, len(devs))
	for _, dv := range devs {
		on := d.online(dv.ID, now)
		if on {
			online++
		}
		devices = append(devices, deviceJSON{dv.ID, dv.Name, dv.LastSeen, on, dv.Principal, dv.Revoked})
	}
	stats, err := d.db.ShareStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	shares := make([]shareJSON, 0, len(stats))
	for _, s := range stats {
		shares = append(shares, shareJSON{s.Name, s.Head, s.Snapshots, s.Members, s.ACLMode, s.UpdatedAt})
	}
	bytes, _ := d.db.SumChunkBytes()
	count := func(t string) int64 { n, _ := d.db.Count(t); return n }
	resp := stateResp{
		Hub:     hubInfo{Version: d.version, UptimeS: int64(time.Since(d.started).Seconds())},
		Totals:  totalsInfo{count("devices"), online, count("shares"), count("snapshots"), count("chunks"), bytes},
		Devices: devices, Shares: shares, History: d.history(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleEvents streams live flow events as SSE (mirrors the hub's client event
// stream; copied not shared since the payload type differs).
func (d *Dashboard) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := d.broker.sub()
	defer d.broker.unsub(ch)
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

// broker fans dashboard events out to connected SSE subscribers; a slow one is
// dropped rather than blocking a hub handler. ponytail: same shape as the hub's
// proto.Event broker — kept separate (different payload) over a generic seam.
type broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan Event]struct{}{}} }

func (b *broker) sub() chan Event {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsub(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *broker) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber; drop (the dashboard re-fetches /api/state)
		}
	}
}
