package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
)

// GetBlob must reject bytes whose BLAKE3 hash doesn't match the requested key —
// a corrupt/truncated transfer or a malicious hub can't slip wrong content in.
func TestGetBlobVerifiesIntegrity(t *testing.T) {
	good := []byte("the real chunk bytes")
	goodHash := chunk.Hash(good)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("TAMPERED payload, wrong content")) // not what goodHash addresses
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("x")
	if _, err := c.GetBlob(goodHash); err == nil {
		t.Fatal("GetBlob accepted bytes that don't match the content hash")
	}

	// Sanity: a server returning the correct bytes verifies cleanly.
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(good)
	}))
	defer srvOK.Close()
	c2 := New(srvOK.URL)
	c2.SetBearer("x")
	b, err := c2.GetBlob(goodHash)
	if err != nil || string(b) != string(good) {
		t.Fatalf("GetBlob(valid) = %q, %v", b, err)
	}
}
