// Package dashboard is the hub's OPTIONAL live dashboard: an embedded web UI
// plus a JSON state snapshot (/api/state) and a Server-Sent-Events flow stream
// (/api/events) that animates devices pushing/joining in real time. It is read-
// only and served on its own address (localhost by default) so it never widens
// the hub's API surface; a nil *Dashboard means "disabled" and is safe to Emit to.
package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
)

//go:embed index.html
var indexHTML []byte

const onlineWindow = 2 * time.Minute

// Event is one live activity event streamed to the dashboard UI. It is NOT wire-
// versioned (UI feed only), so it can change freely without breaking clients.
type Event struct {
	TS         int64  `json:"ts"`   // unix millis
	Type       string `json:"type"` // "join" | "push"
	Device     string `json:"device"`
	DeviceName string `json:"device_name"`
	Share      string `json:"share,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	NewHead    bool   `json:"new_head,omitempty"`
}

// Dashboard holds the live state broker and the read handles into the metadata DB.
type Dashboard struct {
	db      *meta.DB
	version string
	started time.Time
	broker  *broker

	mu       sync.Mutex
	lastSeen map[string]int64 // deviceID -> unix seconds of its last activity
}

// New builds a dashboard over the hub's metadata DB.
func New(db *meta.DB, version string) *Dashboard {
	return &Dashboard{db: db, version: version, started: time.Now(), broker: newBroker(), lastSeen: map[string]int64{}}
}

// Emit records device activity and fans the event out to connected dashboards.
// Safe on a nil receiver (dashboard disabled) so hub handlers call it freely.
func (d *Dashboard) Emit(ev Event) {
	if d == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	if ev.Device != "" {
		d.mu.Lock()
		d.lastSeen[ev.Device] = time.Now().Unix()
		d.mu.Unlock()
	}
	d.broker.publish(ev)
}

func (d *Dashboard) online(deviceID, _ string, now int64) bool {
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
	return mux
}

type stateResp struct {
	Hub     hubInfo      `json:"hub"`
	Totals  totalsInfo   `json:"totals"`
	Devices []deviceJSON `json:"devices"`
	Shares  []shareJSON  `json:"shares"`
}
type hubInfo struct {
	Version string `json:"version"`
	UptimeS int64  `json:"uptime_s"`
}
type totalsInfo struct {
	Devices, Online, Shares, Snapshots, Chunks int64
	Bytes                                      int64
}

// MarshalJSON keeps the totals keys lowercase to match the frontend contract.
func (t totalsInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int64{
		"devices": t.Devices, "online": t.Online, "shares": t.Shares,
		"snapshots": t.Snapshots, "chunks": t.Chunks, "bytes": t.Bytes,
	})
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
		on := d.online(dv.ID, dv.Name, now)
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
		Hub:    hubInfo{Version: d.version, UptimeS: int64(time.Since(d.started).Seconds())},
		Totals: totalsInfo{count("devices"), online, count("shares"), count("snapshots"), count("chunks"), bytes},
		Devices: devices, Shares: shares,
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
