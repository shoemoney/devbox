// Package secret is devbox's default-on secret guard: it hard-refuses to sync
// files whose paths match a built-in deny-list (plus user extras), independent
// of .devignore. This is the "secrets never leave the machine" guarantee.
package secret

import "git.shoemoney.ai/shoemoney/devbox/internal/ignore"

// DefaultPatterns are always blocked from upload. Gitignore syntax, so the
// .env.example re-include works via last-match-wins. Users can un-block a
// default with a negation in their extra patterns (e.g. "!config.pem").
var DefaultPatterns = []string{
	".env",
	".env.*",
	"*.env", // prod.env / config.env conventions hold the same creds as .env
	".envrc",
	"!.env.example",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"*.kdbx",
	"*.ppk",
	"id_rsa*",
	"id_dsa*",
	"id_ecdsa*",
	"id_ed25519*",
	"secrets/",
	"**/.aws/credentials", // not just at root — a nested copy is just as sensitive
	"**/.ssh/id_*",
}

// Guard decides whether a path must never be uploaded.
type Guard struct{ m *ignore.Matcher }

// New compiles the default deny-list plus any extra gitignore-syntax patterns.
func New(extra []string) (*Guard, error) {
	pats := make([]string, 0, len(DefaultPatterns)+len(extra))
	pats = append(pats, DefaultPatterns...)
	pats = append(pats, extra...)
	m, err := ignore.CompileFold(pats) // secrets block case-insensitively (.ENV == .env)
	if err != nil {
		return nil, err
	}
	return &Guard{m: m}, nil
}

// Blocked reports whether path (forward-slash, relative to the share root) is a
// secret that must not be uploaded. It checks the path as both a file and a
// directory name so a dir-only pattern like "secrets/" also blocks a regular
// file literally named "secrets" — the secret guarantee should never slip.
func (g *Guard) Blocked(path string) bool {
	return g.m.Match(path, false) || g.m.Match(path, true)
}
