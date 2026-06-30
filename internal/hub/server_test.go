package hub

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/pkg/proto"
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

// TestPushWriteGate proves the M8a role gate on the HTTP push path: a legacy
// share behaves like v1 (the publisher can push), flipping it to explicit ACLs
// denies an unmembered device (403), a viewer grant is still too low (403), and
// an editor grant opens the gate (the push then fails LATER for a missing blob,
// not 403).
func TestPushWriteGate(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()

	if err := db.CreateToken(HashToken("tok"), now+3600); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: "tok", Name: "laptop", Pubkey: pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge("tok", pub)),
	}))
	var join proto.JoinResponse
	if err := json.Unmarshal(body, &join); err != nil {
		t.Fatal(err)
	}
	if status, b := do(t, "POST", base+proto.PathPublish, join.Bearer, mustJSON(t, proto.PublishRequest{Share: "proj"})); status != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", status, b)
	}

	// A push referencing a blob that was never uploaded — so the only thing under
	// test is the gate: a pass yields 409 (missing blob), a denial yields 403.
	push := func() int {
		st, _ := do(t, "POST", base+proto.PathPush, join.Bearer, mustJSON(t, proto.PushRequest{
			Share:        "proj",
			ManifestHash: strings.Repeat("a", 64),
		}))
		return st
	}

	// Legacy share: the publisher (principal 'owner') pushes — gate passes (409).
	if st := push(); st == http.StatusForbidden {
		t.Fatal("legacy share must allow the owner-device to push (v1 behavior)")
	}
	// Flip to explicit by granting a DIFFERENT principal; our device ('owner') is
	// now unmembered -> deny-by-default.
	if err := db.SetMember("proj", "stranger", meta.RoleEditor, false); err != nil {
		t.Fatal(err)
	}
	if st := push(); st != http.StatusForbidden {
		t.Fatalf("explicit share must deny an unmembered device, got %d", st)
	}
	// Viewer is below editor -> still denied.
	if err := db.SetMember("proj", "owner", meta.RoleViewer, false); err != nil {
		t.Fatal(err)
	}
	if st := push(); st != http.StatusForbidden {
		t.Fatalf("viewer must not be able to push, got %d", st)
	}
	// Editor opens the gate -> no longer 403 (now 409 for the missing blob).
	if err := db.SetMember("proj", "owner", meta.RoleEditor, false); err != nil {
		t.Fatal(err)
	}
	if st := push(); st == http.StatusForbidden {
		t.Fatal("editor must pass the write gate")
	}
}

// TestMetricsCountersAndHealthVersion proves /healthz reports the build version
// and /metrics exposes the new transfer counters, moved by a real blob PUT/GET.
func TestMetricsCountersAndHealthVersion(t *testing.T) {
	db, err := meta.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := blobstore.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(db, store).SetVersion("v9.9.9").Handler())
	defer srv.Close()
	base := srv.URL

	if st, body := do(t, "GET", base+"/healthz", "", nil); st != http.StatusOK || !strings.Contains(string(body), "v9.9.9") {
		t.Fatalf("/healthz = %d %q, want 200 with version", st, body)
	}

	if err := db.CreateToken(HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	bearer := joinWith(t, base, "tok").Bearer
	blob := []byte("metricsblob") // 11 bytes
	h := chunk.Hash(blob)
	if st, _ := do(t, "PUT", base+proto.PathBlob+h, bearer, blob); st != http.StatusOK {
		t.Fatalf("PUT blob = %d", st)
	}
	if st, got := do(t, "GET", base+proto.PathBlob+h, bearer, nil); st != http.StatusOK || !bytes.Equal(got, blob) {
		t.Fatalf("GET blob = %d", st)
	}

	_, m := do(t, "GET", base+proto.PathMetrics, "", nil)
	for _, want := range []string{
		"devbox_blob_bytes_in_total 11",
		"devbox_blob_bytes_out_total 11",
		"devbox_pushes_total",
		"devbox_conflicts_total",
	} {
		if !strings.Contains(string(m), want) {
			t.Fatalf("/metrics missing %q\n---\n%s", want, m)
		}
	}
}

// TestBlobGzipUpload is the hub side of opt-in transport compression: a blob
// PUT with Content-Encoding: gzip must be decompressed before hashing/storing
// (so dedup + integrity are unchanged), round-trip back identical, and a gzip
// header over a non-gzip body must be rejected (bomb-safe ingest).
func TestBlobGzipUpload(t *testing.T) {
	base, db := testHub(t)
	if err := db.CreateToken(HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	bearer := joinWith(t, base, "tok").Bearer

	original := bytes.Repeat([]byte("devbox compresses well over WAN! "), 2048)
	hash := chunk.Hash(original)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(original); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	if gz.Len() >= len(original) {
		t.Fatalf("test data must compress: gz=%d orig=%d", gz.Len(), len(original))
	}

	put := func(h string, body []byte, enc string) int {
		req, _ := http.NewRequest("PUT", base+proto.PathBlob+h, bytes.NewReader(body))
		req.Header.Set(proto.AuthHeader, "Bearer "+bearer)
		if enc != "" {
			req.Header.Set("Content-Encoding", enc)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if st := put(hash, gz.Bytes(), "gzip"); st != http.StatusOK {
		t.Fatalf("gzip PUT status = %d, want 200", st)
	}
	// Hub stored the DECOMPRESSED bytes — GET returns the original.
	if st, got := do(t, "GET", base+proto.PathBlob+hash, bearer, nil); st != http.StatusOK || !bytes.Equal(got, original) {
		t.Fatalf("round-trip: status=%d, %d bytes (want 200, %d)", st, len(got), len(original))
	}
	// A gzip header over a non-gzip body is rejected, not stored.
	if st := put(chunk.Hash([]byte("x")), []byte("not gzip at all"), "gzip"); st != http.StatusBadRequest {
		t.Fatalf("non-gzip body with gzip header: status = %d, want 400", st)
	}
}

// joinWith enrolls a fresh device using token and returns its JoinResponse.
func joinWith(t *testing.T, base, token string) proto.JoinResponse {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	st, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: token, Name: "dev", Pubkey: pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge(token, pub)),
	}))
	if st != http.StatusOK {
		t.Fatalf("join status = %d, body = %s", st, body)
	}
	var j proto.JoinResponse
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// TestInviteFlow is the M8a device-facing invite security contract: an owner
// invites a collaborator, who joins via the invite token and lands with the bound
// role; the legacy→explicit flip preserves the owner's access; and attenuation is
// enforced (an editor without +s can't invite).
func TestInviteFlow(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()

	// Owner device joins a legacy share and publishes it.
	if err := db.CreateToken(HashToken("owntok"), now+3600); err != nil {
		t.Fatal(err)
	}
	owner := joinWith(t, base, "owntok")
	if st, _ := do(t, "POST", base+proto.PathPublish, owner.Bearer, mustJSON(t, proto.PublishRequest{Share: "proj"})); st != http.StatusOK {
		t.Fatal("publish failed")
	}

	// Owner invites principal "bob" as editor.
	st, body := do(t, "POST", base+proto.PathInvite, owner.Bearer, mustJSON(t, proto.InviteRequest{
		Share: "proj", Principal: "bob", Role: "editor",
	}))
	if st != http.StatusOK {
		t.Fatalf("invite status = %d, body = %s", st, body)
	}
	var invResp proto.InviteResponse
	if err := json.Unmarshal(body, &invResp); err != nil {
		t.Fatal(err)
	}

	// The share flipped to explicit but the OWNER kept access (self-seed).
	if mode, _ := db.ACLMode("proj"); mode != "explicit" {
		t.Fatalf("share should be explicit after invite, got %q", mode)
	}
	bogus := mustJSON(t, proto.PushRequest{Share: "proj", ManifestHash: strings.Repeat("a", 64)})
	if st, _ := do(t, "POST", base+proto.PathPush, owner.Bearer, bogus); st == http.StatusForbidden {
		t.Fatal("owner must keep write access across the legacy→explicit flip")
	}

	// Bob's device redeems the invite → enrolled as principal bob with editor.
	bob := joinWith(t, base, invResp.Token)
	if pid, _ := db.DevicePrincipal(bob.DeviceID); pid != "bob" {
		t.Fatalf("invited device principal = %q, want bob", pid)
	}
	if st, _ := do(t, "POST", base+proto.PathPush, bob.Bearer, bogus); st == http.StatusForbidden {
		t.Fatal("invited editor must pass the write gate")
	}

	// Attenuation: bob (editor, NO +s) cannot invite anyone.
	if st, _ := do(t, "POST", base+proto.PathInvite, bob.Bearer, mustJSON(t, proto.InviteRequest{
		Share: "proj", Principal: "carol", Role: "viewer",
	})); st != http.StatusForbidden {
		t.Fatalf("editor without +s must not invite, got %d", st)
	}
}

// TestHandleMembers covers GET /v1/members: a fresh share reads as legacy, and
// after a grant it lists the member with the role name and reshare bit.
func TestHandleMembers(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()
	if err := db.CreateToken(HashToken("tok"), now+3600); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: "tok", Name: "laptop", Pubkey: pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge("tok", pub)),
	}))
	var join proto.JoinResponse
	if err := json.Unmarshal(body, &join); err != nil {
		t.Fatal(err)
	}
	if st, _ := do(t, "POST", base+proto.PathPublish, join.Bearer, mustJSON(t, proto.PublishRequest{Share: "proj"})); st != http.StatusOK {
		t.Fatalf("publish = %d", st)
	}

	get := func() proto.MembersResponse {
		st, b := do(t, "GET", base+proto.PathMembers+"?share=proj", join.Bearer, nil)
		if st != http.StatusOK {
			t.Fatalf("members status = %d, body = %s", st, b)
		}
		var resp proto.MembersResponse
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := get(); !resp.Legacy || len(resp.Members) != 0 {
		t.Fatalf("fresh share should be legacy with no members, got %+v", resp)
	}
	if err := db.SetMember("proj", "bob", meta.RoleEditor, true); err != nil {
		t.Fatal(err)
	}
	resp := get()
	if resp.Legacy || len(resp.Members) != 1 {
		t.Fatalf("after grant want explicit + 1 member, got %+v", resp)
	}
	if m := resp.Members[0]; m.Principal != "bob" || m.Role != "editor" || !m.CanReshare {
		t.Fatalf("member = %+v, want bob/editor/+s", m)
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
