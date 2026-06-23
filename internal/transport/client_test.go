package transport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// writeJSON is a tiny helper for the stub hub.
func writeJSON(t *testing.T, w http.ResponseWriter, code int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestJoinSetsBearer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != proto.PathJoin {
			t.Fatalf("Join: got %s %s", r.Method, r.URL.Path)
		}
		if h := r.Header.Get(proto.AuthHeader); h != "" {
			t.Fatalf("Join must not send auth header, got %q", h)
		}
		var req proto.JoinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode JoinRequest: %v", err)
		}
		if req.Token != "tok" || req.Name != "laptop" || !bytes.Equal(req.Pubkey, pub) {
			t.Fatalf("unexpected JoinRequest: %+v", req)
		}
		// The client must prove possession: a valid signature over the challenge.
		if !ed25519.Verify(pub, proto.JoinChallenge(req.Token, req.Pubkey), req.Signature) {
			t.Fatal("Join did not send a valid proof-of-possession signature")
		}
		writeJSON(t, w, http.StatusOK, proto.JoinResponse{DeviceID: "dev1", Bearer: "secret"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.Join("tok", "laptop", pub, priv)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if resp.DeviceID != "dev1" || resp.Bearer != "secret" {
		t.Fatalf("unexpected JoinResponse: %+v", resp)
	}
	if c.Bearer() != "secret" {
		t.Fatalf("Join should have set bearer, got %q", c.Bearer())
	}
}

// requireBearer asserts an authenticated request carries the expected header.
func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if h := r.Header.Get(proto.AuthHeader); h != "Bearer secret" {
		t.Fatalf("%s %s: bad auth header %q", r.Method, r.URL.Path, h)
	}
}

func TestPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != proto.PathPublish {
			t.Fatalf("Publish: got %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r)
		var req proto.PublishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode PublishRequest: %v", err)
		}
		if req.Share != "proj" {
			t.Fatalf("unexpected share %q", req.Share)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")
	if err := c.Publish("proj"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestHave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != proto.PathHave {
			t.Fatalf("Have: got %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r)
		var req proto.HaveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode HaveRequest: %v", err)
		}
		if len(req.Hashes) != 2 || req.Hashes[0] != "a" || req.Hashes[1] != "b" {
			t.Fatalf("unexpected hashes: %v", req.Hashes)
		}
		writeJSON(t, w, http.StatusOK, proto.HaveResponse{Missing: []string{"b"}})
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")
	missing, err := c.Have([]string{"a", "b"})
	if err != nil {
		t.Fatalf("Have: %v", err)
	}
	if len(missing) != 1 || missing[0] != "b" {
		t.Fatalf("unexpected missing: %v", missing)
	}
}

func TestPutBlob(t *testing.T) {
	const hash = "deadbeef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("PutBlob: got method %s", r.Method)
		}
		if r.URL.Path != proto.PathBlob+hash {
			t.Fatalf("PutBlob: got path %s, want %s", r.URL.Path, proto.PathBlob+hash)
		}
		requireBearer(t, r)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read blob body: %v", err)
		}
		if string(body) != "blobby" {
			t.Fatalf("PutBlob: got body %q, want %q", body, "blobby")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")
	if err := c.PutBlob(hash, []byte("blobby")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
}

func TestPush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != proto.PathPush {
			t.Fatalf("Push: got %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r)
		var req proto.PushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode PushRequest: %v", err)
		}
		if req.Share != "proj" || req.ManifestHash != "mhash" || len(req.Chunks) != 1 {
			t.Fatalf("unexpected PushRequest: %+v", req)
		}
		writeJSON(t, w, http.StatusOK, proto.PushResponse{Snapshot: "snap1", Head: "snap1"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")
	resp, err := c.Push(proto.PushRequest{
		Share:        "proj",
		ManifestHash: "mhash",
		Chunks:       []proto.ChunkRef{{Hash: "a", Size: 3}},
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if resp.Snapshot != "snap1" || resp.Head != "snap1" {
		t.Fatalf("unexpected PushResponse: %+v", resp)
	}
}

func TestHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != proto.PathHead {
			t.Fatalf("Head: got %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r)
		if got := r.URL.Query().Get("share"); got != "proj" {
			t.Fatalf("Head: got share %q, want proj", got)
		}
		writeJSON(t, w, http.StatusOK, proto.HeadResponse{Head: "snap1"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")
	head, err := c.Head("proj")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "snap1" {
		t.Fatalf("Head: got %q, want snap1", head)
	}
}

func TestErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, proto.Error{Error: "share exists"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetBearer("secret")

	err := c.Publish("proj")
	if err == nil {
		t.Fatal("Publish: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "share exists") || !strings.Contains(err.Error(), "409") {
		t.Fatalf("Publish error missing message/status: %v", err)
	}

	if _, err := c.Push(proto.PushRequest{Share: "proj"}); err == nil {
		t.Fatal("Push: expected error, got nil")
	} else if !strings.Contains(err.Error(), "share exists") {
		t.Fatalf("Push error missing message: %v", err)
	}
}
