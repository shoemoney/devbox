package hub

import "testing"

func TestValidateShareName(t *testing.T) {
	existing := []string{"projects", "Repos"}
	cases := []struct {
		name string
		ok   bool
	}{
		{"projects", true},  // exact match of an existing share is fine (idempotent publish)
		{"my-app_2", true},  // letters, digits, dash, underscore
		{"web.staging", true},
		{"", false},         // empty
		{" lead", false},    // whitespace
		{"trail ", false},
		{"a/b", false},      // path separator (sub-paths are mount-side, not in the name)
		{"a\\b", false},     // backslash
		{"..", false},       // traversal
		{".hidden", false},  // leading dot
		{"trail.", false},   // trailing dot (Windows strips it)
		{"-flag", false},    // leading dash
		{"sp ace", false},   // interior space
		{"emoji🎉", false},  // non-ASCII
		{"CON", false},      // reserved Windows device name
		{"lpt3", false},     // reserved (case-insensitive)
		{"repos", false},    // case-clashes with existing "Repos"
		{"REPOS", false},    // case-clash
	}
	for _, c := range cases {
		err := ValidateShareName(c.name, existing)
		if c.ok && err != nil {
			t.Errorf("ValidateShareName(%q) = %v, want ok", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateShareName(%q) = ok, want error", c.name)
		}
	}
}
