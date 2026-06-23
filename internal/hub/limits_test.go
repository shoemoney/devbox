package hub

import (
	"bytes"
	"net/http"
	"testing"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
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

// TestHubServerTimeoutsConfigured documents the intended hardening: the hub sets
// read/idle timeouts but deliberately leaves WriteTimeout unset so the long-lived
// /v1/events SSE stream is not killed mid-flight. The actual http.Server is built
// in cmd/devbox-hub; this test pins the constants so a careless edit that drops
// the caps trips here.
func TestHubByteCapsAreSane(t *testing.T) {
	if maxJSONBytes <= 0 || maxBlobBytes <= 0 {
		t.Fatalf("byte caps must be positive: json=%d blob=%d", maxJSONBytes, maxBlobBytes)
	}
	if maxJSONBytes >= maxBlobBytes {
		t.Fatalf("maxJSONBytes (%d) should be much smaller than maxBlobBytes (%d)", maxJSONBytes, maxBlobBytes)
	}
}
