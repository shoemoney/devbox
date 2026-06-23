package secret

import "testing"

func TestDefaultGuard(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		".env", ".env.local", ".env.production",
		"key.pem", "server.key", "cert.p12", "store.pfx", "vault.kdbx",
		"id_rsa", "id_ed25519", "id_ed25519.pub",
		"secrets/aws.json", "secrets/sub/token", ".aws/credentials", ".ssh/id_rsa",
		"secrets", // a regular file literally named "secrets" must also be blocked
	}
	for _, p := range blocked {
		if !g.Blocked(p) {
			t.Errorf("expected %q to be BLOCKED", p)
		}
	}
	allowed := []string{
		".env.example", "main.go", "README.md", "src/app.ts",
		"public.cert", "notes.txt", "config.yaml",
	}
	for _, p := range allowed {
		if g.Blocked(p) {
			t.Errorf("expected %q to be ALLOWED", p)
		}
	}
}

func TestExtraPatternsAndUnblock(t *testing.T) {
	g, err := New([]string{"*.secretz", "!server.key"})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Blocked("creds.secretz") {
		t.Error("extra pattern *.secretz should block")
	}
	// a user negation un-blocks a default-blocked path
	if g.Blocked("server.key") {
		t.Error("!server.key extra should un-block the default *.key match")
	}
}
