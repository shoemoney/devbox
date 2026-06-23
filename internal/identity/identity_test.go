package identity

import (
	"crypto/ed25519"
	"testing"
)

func TestRoundTripAndIdempotent(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.Fingerprint() == "" {
		t.Fatal("empty fingerprint")
	}

	// Load reproduces the same identity.
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint() != id.Fingerprint() {
		t.Fatalf("fingerprint changed on load: %s != %s", got.Fingerprint(), id.Fingerprint())
	}

	// LoadOrCreate must NOT regenerate an existing identity.
	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint() != id.Fingerprint() {
		t.Fatal("LoadOrCreate regenerated an existing identity")
	}

	// The loaded key actually signs/verifies.
	msg := []byte("devbox")
	sig := ed25519.Sign(again.Priv, msg)
	if !ed25519.Verify(again.Pub, msg, sig) {
		t.Fatal("sign/verify failed for loaded key")
	}
}

func TestFingerprintDiffersPerKey(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("distinct keys produced identical fingerprints")
	}
}
