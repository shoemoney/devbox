package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/pkg/proto"
)

// TestCloseEventStreams proves a live /v1/events SSE stream returns promptly when
// CloseEventStreams fires (what RegisterOnShutdown calls) — otherwise a graceful
// shutdown would block the full drain timeout on streams that never go idle.
func TestCloseEventStreams(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, store)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if err := db.CreateToken(HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	bearer := joinWith(t, srv.URL, "tok").Bearer

	req, _ := http.NewRequest(http.MethodGet, srv.URL+proto.PathEvents, nil)
	req.Header.Set(proto.AuthHeader, "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", resp.StatusCode)
	}

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()

	s.CloseEventStreams() // the RegisterOnShutdown hook

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("event stream did not return promptly after CloseEventStreams")
	}
}
