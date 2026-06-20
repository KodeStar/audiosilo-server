package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SafeJoin is the single security primitive gating every filesystem access
// derived from user input, so it gets explicit allow + deny coverage.
func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	rootAbs, _ := filepath.EvalSymlinks(root)

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

// TestSafeJoinSymlinkEscape covers review finding S1: a symlink inside the root
// that points outside it must be rejected (not followed), while a symlink that
// stays inside the root is still allowed.
func TestSafeJoinSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling directory outside the root
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the root pointing outside it must be rejected.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if got, err := SafeJoin(root, "escape/secret.txt"); err == nil {
		t.Fatalf("SafeJoin followed a symlink out of the root: %q", got)
	}

	// A symlink that stays inside the root is fine.
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "inside")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if _, err := SafeJoin(root, "inside"); err != nil {
		t.Fatalf("SafeJoin rejected an in-root symlink: %v", err)
	}
}
