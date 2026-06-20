package library

import (
	"path/filepath"
	"strings"
	"testing"
)

// SafeJoin is the single security primitive gating every filesystem access
// derived from user input, so it gets explicit allow + deny coverage.
func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	rootAbs, _ := filepath.Abs(root)

	cases := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"empty stays at root", "", false},
		{"simple nested", "Author/Book/01.m4b", false},
		{"dot segments collapse", "Author/./Book", false},
		{"interior parent that stays inside", "Author/Book/../Book/01.m4b", false},
		{"parent traversal rejected", "../etc", true},
		{"deep traversal rejected", "Author/../../etc/passwd", true},
		{"long traversal rejected", "../../../../../../etc/passwd", true},
		// An absolute relPath is treated as relative to the root by filepath.Join,
		// so it stays contained rather than escaping.
		{"absolute path is contained", "/etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeJoin(root, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SafeJoin(%q) = %q, want error", tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeJoin(%q) unexpected error: %v", tc.rel, err)
			}
			if got != rootAbs && !strings.HasPrefix(got, rootAbs+string(filepath.Separator)) {
				t.Fatalf("SafeJoin(%q) = %q escaped root %q", tc.rel, got, rootAbs)
			}
		})
	}
}

// NOTE (review finding S1): SafeJoin does lexical containment only — it does not
// call filepath.EvalSymlinks, so a symlink *inside* the root that points outside
// is currently followed. Low risk today (no API lets a non-operator create
// symlinks), but harden with EvalSymlinks before Phase B uploads land. When that
// fix is made, add a symlink-escape case to TestSafeJoin above.
