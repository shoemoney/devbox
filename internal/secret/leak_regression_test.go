package secret

import "testing"

// Regression for the audit's credential-leak findings: the secret guard must
// block these despite case, .env naming variants, and nesting depth.
func TestSecretGuardBlocksLeakyNames(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		// #5 case-insensitive (macOS/Windows): same secret, different case.
		".ENV", ".Env", "KEY.PEM", "cert.PEM", "ID_RSA", "Server.Key",
		// #17 common env-file naming conventions.
		"prod.env", "config.env", ".envrc",
		// #16 nested copies of anchored secrets.
		"home/.aws/credentials", "deep/path/.aws/credentials", "x/.ssh/id_work",
		// sanity: the originals still block.
		".env", "id_rsa", "secrets/token",
	}
	for _, p := range blocked {
		if !g.Blocked(p) {
			t.Errorf("Blocked(%q) = false, want true (credential leak)", p)
		}
	}

	// The non-secret template must stay allowed (negation still works).
	if g.Blocked(".env.example") {
		t.Error("Blocked(.env.example) = true, want false (it's a template, not a secret)")
	}
}
