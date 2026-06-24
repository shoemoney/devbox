package hub

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/pkg/proto"
)

// The request-size caps (maxBlobBytes/maxJSONBytes) are enforced by
// http.MaxBytesReader, so correctness is mostly by construction — exercising the
// 256 MiB / 8 MiB limits for real would be far too heavy for a unit test. This is
// a regression guard that the wiring didn't break the happy path: a normal-sized
// blob PUT and a normal-sized JSON request still succeed under the caps.
func TestHubRequestsUnderLimitsSucceed(t *testing.T) {
	base, db := testHub(t)
	bearer := enroll(t, base, db)

	// A normal blob (well under maxBlobBytes) round-trips through MaxBytesReader.
	blob := bytes.Repeat([]byte("x"), 4096)
	hash := chunk.Hash(blob)
	if status, body := do(t, "PUT", base+proto.PathBlob+hash, bearer, blob); status != http.StatusOK {
		t.Fatalf("PUT blob under cap = %d, want 200; body=%s", status, body)
	}

	// A normal JSON control request (well under maxJSONBytes) still decodes.
	if status, body := do(t, "POST", base+proto.PathHave, bearer,
		mustJSON(t, proto.HaveRequest{Hashes: []string{hash}})); status != http.StatusOK {
		t.Fatalf("POST have under cap = %d, want 200; body=%s", status, body)
	}
}
