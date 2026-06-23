package hub

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
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// testHub spins a live httptest.Server backed by an in-memory meta DB and a
// temp-dir blobstore. It returns the base URL and the DB for assertions.
func testHub(t *testing.T) (string, *meta.DB) {
	t.Helper()
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}
	srv := httptest.NewServer(NewServer(db, store).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, db
}

// do issues a request and returns its status and body. authBearer is sent as a
// Bearer token when non-empty.
func do(t *testing.T, method, url, authBearer string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if authBearer != "" {
		req.Header.Set(proto.AuthHeader, "Bearer "+authBearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHubHappyPath(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()

	// 1. Mint a join token, then join with a real ed25519 pubkey.
	if err := db.CreateToken(HashToken("jointok"), now+3600); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	status, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token:     "jointok",
		Name:      "laptop",
		Pubkey:    pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge("jointok", pub)),
	}))
	if status != http.StatusOK {
		t.Fatalf("join status = %d, body = %s", status, body)
	}
	var join proto.JoinResponse
	if err := json.Unmarshal(body, &join); err != nil {
		t.Fatalf("decode join: %v", err)
	}
	if join.DeviceID == "" || join.Bearer == "" {
		t.Fatalf("join returned empty fields: %+v", join)
	}
	bearer := join.Bearer

	if n, err := db.Count("devices"); err != nil || n != 1 {
		t.Fatalf("device count = %d (err %v), want 1", n, err)
	}

	// 2. Publish a share; then have/upload two chunk blobs.
	const share = "proj"
	if status, body := do(t, "POST", base+proto.PathPublish, bearer, mustJSON(t, proto.PublishRequest{Share: share})); status != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", status, body)
	}

	blobA := bytes.Repeat([]byte("a"), 100)
	blobB := bytes.Repeat([]byte("b"), 200)
	hashA, hashB := chunk.Hash(blobA), chunk.Hash(blobB)

	status, body = do(t, "POST", base+proto.PathHave, bearer, mustJSON(t, proto.HaveRequest{Hashes: []string{hashA, hashB}}))
	if status != http.StatusOK {
		t.Fatalf("have status = %d, body = %s", status, body)
	}
	var have proto.HaveResponse
	if err := json.Unmarshal(body, &have); err != nil {
		t.Fatalf("decode have: %v", err)
	}
	if len(have.Missing) != 2 {
		t.Fatalf("missing = %v, want both", have.Missing)
	}

	for h, b := range map[string][]byte{hashA: blobA, hashB: blobB} {
		if status, body := do(t, "PUT", base+proto.PathBlob+h, bearer, b); status != http.StatusOK {
			t.Fatalf("PUT blob %s status = %d, body = %s", h, status, body)
		}
	}

	status, body = do(t, "POST", base+proto.PathHave, bearer, mustJSON(t, proto.HaveRequest{Hashes: []string{hashA, hashB}}))
	if status != http.StatusOK {
		t.Fatalf("have2 status = %d, body = %s", status, body)
	}
	have = proto.HaveResponse{}
	if err := json.Unmarshal(body, &have); err != nil {
		t.Fatalf("decode have2: %v", err)
	}
	if len(have.Missing) != 0 {
		t.Fatalf("missing after upload = %v, want none", have.Missing)
	}

	// 3. Upload the manifest blob, then push the snapshot.
	manifest := []byte("manifest-bytes")
	manifestHash := chunk.Hash(manifest)
	if status, body := do(t, "PUT", base+proto.PathBlob+manifestHash, bearer, manifest); status != http.StatusOK {
		t.Fatalf("PUT manifest status = %d, body = %s", status, body)
	}

	status, body = do(t, "POST", base+proto.PathPush, bearer, mustJSON(t, proto.PushRequest{
		Share:        share,
		Parent:       "",
		ManifestHash: manifestHash,
		Chunks:       []proto.ChunkRef{{Hash: hashA, Size: 100}, {Hash: hashB, Size: 200}},
	}))
	if status != http.StatusOK {
		t.Fatalf("push status = %d, body = %s", status, body)
	}
	var push proto.PushResponse
	if err := json.Unmarshal(body, &push); err != nil {
		t.Fatalf("decode push: %v", err)
	}
	if push.Snapshot != manifestHash || push.Head != manifestHash || push.Conflict {
		t.Fatalf("push response = %+v, want snapshot/head == %s", push, manifestHash)
	}

	// GET head reflects the new snapshot.
	status, body = do(t, "GET", base+proto.PathHead+"?share="+share, bearer, nil)
	if status != http.StatusOK {
		t.Fatalf("head status = %d, body = %s", status, body)
	}
	var head proto.HeadResponse
	if err := json.Unmarshal(body, &head); err != nil {
		t.Fatalf("decode head: %v", err)
	}
	if head.Head != manifestHash {
		t.Fatalf("head = %q, want %q", head.Head, manifestHash)
	}

	// GET log returns the share's snapshot history (one entry so far).
	status, body = do(t, "GET", base+proto.PathLog+"?share="+share, bearer, nil)
	if status != http.StatusOK {
		t.Fatalf("log status = %d, body = %s", status, body)
	}
	var log proto.LogResponse
	if err := json.Unmarshal(body, &log); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if len(log.Snapshots) != 1 || log.Snapshots[0].ID != manifestHash {
		t.Fatalf("log = %+v, want one snapshot %s", log.Snapshots, manifestHash)
	}

	// /metrics exposes the gauges.
	status, body = do(t, "GET", base+proto.PathMetrics, "", nil)
	if status != http.StatusOK {
		t.Fatalf("metrics status = %d", status)
	}
	metrics := string(body)
	for _, want := range []string{"# TYPE devbox_devices gauge", "devbox_devices 1", "devbox_shares 1", "devbox_snapshots 1", "devbox_chunks 2"} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, metrics)
		}
	}
}

func TestHubAuthAndHashErrors(t *testing.T) {
	base, db := testHub(t)

	// No bearer -> 401.
	if status, _ := do(t, "POST", base+proto.PathPublish, "", mustJSON(t, proto.PublishRequest{Share: "x"})); status != http.StatusUnauthorized {
		t.Fatalf("no-bearer status = %d, want 401", status)
	}

	// Join to get a valid bearer so we can reach the blob handler.
	now := time.Now().Unix()
	if err := db.CreateToken(HashToken("tok2"), now+3600); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	status, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{Token: "tok2", Name: "n", Pubkey: pub, Signature: ed25519.Sign(priv, proto.JoinChallenge("tok2", pub))}))
	if status != http.StatusOK {
		t.Fatalf("join status = %d, body = %s", status, body)
	}
	var join proto.JoinResponse
	if err := json.Unmarshal(body, &join); err != nil {
		t.Fatalf("decode join: %v", err)
	}

	// Blob body that does not match {hash} -> 400.
	if status, _ := do(t, "PUT", base+proto.PathBlob+"deadbeef", join.Bearer, []byte("not-deadbeef")); status != http.StatusBadRequest {
		t.Fatalf("mismatched blob status = %d, want 400", status)
	}
}
