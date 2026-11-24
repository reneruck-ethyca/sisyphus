package config

import "testing"

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"**/x", "x", true},
		{"**/x", "a/b/x", true},
		{"dir/**", "dir/a/b.ts", true},
		{"a/**/z", "a/b/c/z", true},
		{"dir/**", "other/a.ts", false},
		{"a/*/z", "a/b/c/z", false},
	} {
		if got := GlobMatch(tc.glob, tc.path); got != tc.want {
			t.Errorf("GlobMatch(%q, %q) = %v", tc.glob, tc.path, got)
		}
	}
}
