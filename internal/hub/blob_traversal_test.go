package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// enroll joins a device and returns its bearer token.
func enroll(t *testing.T, base string, db *meta.DB) string {
	t.Helper()
	if err := db.CreateToken(HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	status, body := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: "tok", Name: "dev", Pubkey: pub,
	}))
	if status != http.StatusOK {
		t.Fatalf("join status %d: %s", status, body)
	}
	var jr proto.JoinResponse
	if err := json.Unmarshal(body, &jr); err != nil {
		t.Fatal(err)
	}
	return jr.Bearer
}

// An authenticated device must NOT be able to read files outside the blob root
// via a path-traversal blob hash (ServeMux URL-decodes %2f in the {hash} wildcard).
func TestBlobGetRejectsTraversal(t *testing.T) {
	base, db := testHub(t)
	bearer := enroll(t, base, db)

	for _, key := range []string{
		"..%2f..%2f..%2f..%2fetc%2fpasswd",
		"..%2f..%2fdevbox-hub.db",
		"not-a-hash",
		"deadbeef", // valid hex but wrong length
	} {
		status, body := do(t, "GET", base+proto.PathBlob+key, bearer, nil)
		if status == http.StatusOK {
			t.Errorf("GET blob %q returned 200 (leak): %s", key, body)
		}
	}

	// /v1/have must reject a traversal hash rather than stat an arbitrary path.
	status, _ := do(t, "POST", base+proto.PathHave, bearer,
		mustJSON(t, proto.HaveRequest{Hashes: []string{"../../../../etc/hosts"}}))
	if status != http.StatusBadRequest {
		t.Errorf("have with traversal hash = %d, want 400", status)
	}
}
