package dashboard

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/hub/meta"
)

// TestStateAndEvents covers both dashboard surfaces: /api/state reflects the DB,
// and an Emit'd flow event reaches a connected /api/events subscriber.
func TestStateAndEvents(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	if err := db.AddDevice("dev1", "hueb", []byte("k"), now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("proj", "dev1", now); err != nil {
		t.Fatal(err)
	}

	d := New(db, "1.2.3")
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()

	// /api/state reflects the DB.
	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var st stateResp
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st.Hub.Version != "1.2.3" {
		t.Fatalf("version = %q", st.Hub.Version)
	}
	if len(st.Devices) != 1 || st.Devices[0].Name != "hueb" {
		t.Fatalf("devices = %+v", st.Devices)
	}
	if len(st.Shares) != 1 || st.Shares[0].Name != "proj" {
		t.Fatalf("shares = %+v", st.Shares)
	}

	// / serves the embedded UI.
	if r, _ := http.Get(srv.URL + "/"); r == nil || r.StatusCode != 200 {
		t.Fatal("dashboard root did not serve")
	}

	// A flow event reaches a live /api/events subscriber.
	ev, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Body.Close()
	got := make(chan string, 1)
	go func() {
		br := bufio.NewReader(ev.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.Contains(line, `"type":"push"`) {
				got <- line
				return
			}
		}
	}()
	for i := 0; i < 20; i++ { // emit until the subscriber (registered async) catches one
		d.Emit(Event{Type: "push", Device: "dev1", Share: "proj", Chunks: 3, Bytes: 99})
		select {
		case line := <-got:
			if !strings.Contains(line, "proj") {
				t.Fatalf("event missing share: %s", line)
			}
			return
		case <-time.After(40 * time.Millisecond):
		}
	}
	t.Fatal("did not receive the push event over SSE")
}

// TestEmitNilSafe documents that hub handlers can Emit unconditionally.
func TestEmitNilSafe(t *testing.T) {
	var d *Dashboard
	d.Emit(Event{Type: "push"}) // must not panic
}

// TestHistoryRing covers the server-side sparkline window: per-minute bucketing,
// per-type counting (push/pull/conflict/gc), and trimming to histWindow minutes.
func TestHistoryRing(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Window trim: more minutes than histWindow keeps only the newest histWindow.
	d := New(db, "t")
	base := int64(1_000_020) - int64(1_000_020)%60 // minute-aligned
	d.mu.Lock()
	for i := 0; i < histWindow+5; i++ {
		d.recordLocked(Event{Type: "push", Bytes: 10}, base+int64(i*60))
	}
	d.mu.Unlock()
	h := d.history()
	if len(h) != histWindow {
		t.Fatalf("history len = %d, want %d (trimmed)", len(h), histWindow)
	}
	if h[0].TS != base+int64(5*60) {
		t.Fatalf("oldest retained bucket TS = %d, want %d", h[0].TS, base+5*60)
	}

	// Per-type counting within a single minute bucket; join is not charted.
	d2 := New(db, "t")
	now := int64(2_000_000)
	d2.mu.Lock()
	d2.recordLocked(Event{Type: "push", Bytes: 100}, now)
	d2.recordLocked(Event{Type: "push", Bytes: 50}, now)
	d2.recordLocked(Event{Type: "pull"}, now)
	d2.recordLocked(Event{Type: "conflict"}, now)
	d2.recordLocked(Event{Type: "gc", Pruned: 3, Chunks: 7}, now)
	d2.recordLocked(Event{Type: "join"}, now)
	d2.mu.Unlock()
	h2 := d2.history()
	if len(h2) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(h2))
	}
	if b := h2[0]; b.Pushes != 2 || b.Bytes != 150 || b.Pulls != 1 || b.Conflicts != 1 || b.GCs != 1 {
		t.Fatalf("bucket = %+v, want pushes=2 bytes=150 pulls=1 conflicts=1 gcs=1", b)
	}
}
