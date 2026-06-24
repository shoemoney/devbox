package hub

// P2 adversarial-audit regression tests. Each pins a fix for a confirmed M8a
// finding so a future refactor can't silently reopen it. See docs/M8a-audit.md.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shoemoney/devbox/pkg/proto"
)

// mintInvite asks the hub to mint an invite and returns its raw token.
func mintInvite(t *testing.T, base, bearer string, req proto.InviteRequest) (int, string) {
	t.Helper()
	st, body := do(t, "POST", base+proto.PathInvite, bearer, mustJSON(t, req))
	if st != http.StatusOK {
		return st, ""
	}
	var resp proto.InviteResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	return st, resp.Token
}

// tryJoin redeems token with a fresh keypair and returns the HTTP status.
func tryJoin(t *testing.T, base, token string) int {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := do(t, "POST", base+proto.PathJoin, "", mustJSON(t, proto.JoinRequest{
		Token: token, Name: "dev", Pubkey: pub,
		Signature: ed25519.Sign(priv, proto.JoinChallenge(token, pub)),
	}))
	return st
}

// Finding #2 (privesc): an invite must not confer the +s reshare bit the caller
// doesn't hold. An admin WITHOUT +s could previously mint an invite WITH +s.
func TestInviteCannotGrantReshareCallerLacks(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()
	if err := db.CreateToken(HashToken("own"), now+3600); err != nil {
		t.Fatal(err)
	}
	owner := joinWith(t, base, "own")
	if st, _ := do(t, "POST", base+proto.PathPublish, owner.Bearer, mustJSON(t, proto.PublishRequest{Share: "proj"})); st != http.StatusOK {
		t.Fatal("publish failed")
	}

	// Owner grants principal "adm" admin, explicitly WITHOUT +s.
	if st, _ := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "adm", Role: "admin", Reshare: false}); st != http.StatusOK {
		t.Fatalf("owner→admin invite status=%d", st)
	}
	// adm's device redeems and is enrolled as admin (no +s).
	_, admTok := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "adm2", Role: "admin", Reshare: false})
	adm := joinWith(t, base, admTok)

	// adm (admin, no +s) tries to confer +s → must be denied (the fix).
	if st, _ := mintInvite(t, base, adm.Bearer, proto.InviteRequest{Share: "proj", Principal: "carol", Role: "editor", Reshare: true}); st != http.StatusForbidden {
		t.Fatalf("admin without +s must NOT confer +s, got %d (escalation reopened)", st)
	}
	// Sanity: adm CAN still grant the same role without +s (admins may grant).
	if st, _ := mintInvite(t, base, adm.Bearer, proto.InviteRequest{Share: "proj", Principal: "dave", Role: "editor", Reshare: false}); st != http.StatusOK {
		t.Fatalf("admin must still grant a non-resharing editor, got %d", st)
	}
	// Owner is unconstrained on +s (top of the hierarchy).
	if st, _ := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "erin", Role: "editor", Reshare: true}); st != http.StatusOK {
		t.Fatalf("owner must be able to confer +s, got %d", st)
	}
}

// Finding #1/#5 (replay/pop): a pending invite must be revocable so a leaked or
// regretted invite can be killed before its TTL — and only by someone who could
// have minted it.
func TestInviteRevoke(t *testing.T) {
	base, db := testHub(t)
	now := time.Now().Unix()
	if err := db.CreateToken(HashToken("own"), now+3600); err != nil {
		t.Fatal(err)
	}
	owner := joinWith(t, base, "own")
	if st, _ := do(t, "POST", base+proto.PathPublish, owner.Bearer, mustJSON(t, proto.PublishRequest{Share: "proj"})); st != http.StatusOK {
		t.Fatal("publish failed")
	}

	// Mint an invite, then revoke it; the token must no longer redeem.
	_, tok := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "bob", Role: "editor"})
	st, body := do(t, "POST", base+proto.PathInviteRevoke, owner.Bearer, mustJSON(t, proto.InviteRevokeRequest{Token: tok}))
	if st != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", st, body)
	}
	if got := tryJoin(t, base, tok); got != http.StatusUnauthorized {
		t.Fatalf("revoked invite must not redeem, got %d", got)
	}
	// Revoking again is a no-op conflict/not-found (binding already gone).
	if st, _ := do(t, "POST", base+proto.PathInviteRevoke, owner.Bearer, mustJSON(t, proto.InviteRevokeRequest{Token: tok})); st == http.StatusOK {
		t.Fatal("re-revoking a killed invite must not report success")
	}

	// Authority: a mere viewer cannot revoke an editor invite.
	_, vtok := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "val", Role: "viewer"})
	viewer := joinWith(t, base, vtok)
	_, target := mintInvite(t, base, owner.Bearer, proto.InviteRequest{Share: "proj", Principal: "wanda", Role: "editor"})
	if st, _ := do(t, "POST", base+proto.PathInviteRevoke, viewer.Bearer, mustJSON(t, proto.InviteRevokeRequest{Token: target})); st != http.StatusForbidden {
		t.Fatalf("viewer must not revoke an editor invite, got %d", st)
	}
	// And that target invite still redeems (was not killed by the failed attempt).
	if got := tryJoin(t, base, target); got != http.StatusOK {
		t.Fatalf("invite must survive an unauthorized revoke attempt, got %d", got)
	}
}
