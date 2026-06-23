package hub

import (
	"fmt"
	"strings"
)

// reservedNames are Windows device names a share must not use (case-insensitive):
// a share name can become a directory on a Windows client, where these are magic.
var reservedNames = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
	for i := 1; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}()

// ValidateShareName rejects share names that are unsafe as a path component or
// would collide with an existing share on a case-insensitive filesystem (macOS,
// Windows). existing is the set of names already on the hub; an exact match is
// allowed (publish is idempotent) — only a different-case clash is rejected.
func ValidateShareName(name string, existing []string) error {
	switch {
	case name == "":
		return fmt.Errorf("share name is empty")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("share name %q has leading/trailing whitespace", name)
	case len(name) > 128:
		return fmt.Errorf("share name %q is too long (max 128 bytes)", name)
	case name == "." || name == "..":
		return fmt.Errorf("share name %q is reserved", name)
	case strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-"):
		return fmt.Errorf("share name %q must not start with '.' or '-'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("share name %q has invalid character %q (allowed: letters, digits, . _ -)", name, r)
		}
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("share name %q is a reserved device name", name)
	}
	lower := strings.ToLower(name)
	for _, e := range existing {
		if e != name && strings.ToLower(e) == lower {
			return fmt.Errorf("share name %q case-clashes with existing share %q (collides on case-insensitive filesystems)", name, e)
		}
	}
	return nil
}
