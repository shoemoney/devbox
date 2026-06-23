package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// A join must carry a valid proof-of-possession signature over JoinChallenge,
// and a bad attempt must NOT consume the one-time token (the signature is checked
// before the token is redeemed).
func TestJoinRequiresProofOfPossession(t *testing.T) {
	base, db := testHub(t)
	if err := db.CreateToken(HashToken("tok"), time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	bad := []proto.JoinRequest{
		{Token: "tok", Name: "d", Pubkey: pub, Signature: []byte("garbage")},
		{Token: "tok", Name: "d", Pubkey: pub}, // missing signature
		{Token: "tok", Name: "d", Pubkey: pub, Signature: ed25519.Sign(otherPriv, proto.JoinChallenge("tok", pub))}, // signed by the wrong key
	}
	for i, req := range bad {
		if status, _ := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, req)); status != http.StatusUnauthorized {
			t.Fatalf("bad join %d = %d, want 401", i, status)
		}
	}

	// The token survived every bad attempt — a correct signature still enrolls.
	status, _ := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: "tok", Name: "d", Pubkey: pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge("tok", pub)),
	}))
	if status != http.StatusOK {
		t.Fatalf("valid join after bad attempts = %d, want 200 (one-time token wrongly burned?)", status)
	}
}
