package library

import (
	"fmt"
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

// writeAudioFiles creates n empty .mp3 files under dir (BrowseFS classifies by
// extension via metadata.IsAudio, so the bytes don't matter) and returns the
// dir's library-relative path.
func writeAudioFiles(t *testing.T, root, rel string, n int) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// Zero-padded so the lexical sort order is deterministic.
		name := fmt.Sprintf("track-%04d.mp3", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rel
}

// TestBrowseFSPagination walks a directory with offset pagination and asserts the
// page math: NextOffset advances, every entry is covered exactly once across the
// pages, an offset past the total yields an empty page, and limit<=0 falls back to
// the 200 default.
func TestBrowseFSPagination(t *testing.T) {
	root := t.TempDir()
	const total = 25
	rel := writeAudioFiles(t, root, "Book", total)

	// Page through with a small limit, collecting every entry path exactly once.
	const limit = 10
	seen := map[string]int{}
	offset, pages := 0, 0
	for {
		listing, err := BrowseFS(root, rel, offset, limit, nil)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if listing.Total != total {
			t.Fatalf("page at offset %d: Total = %d, want %d", offset, listing.Total, total)
		}
		if listing.Offset != offset {
			t.Fatalf("page Offset = %d, want %d", listing.Offset, offset)
		}
		if len(listing.Entries) > limit {
			t.Fatalf("page returned %d entries, exceeds limit %d", len(listing.Entries), limit)
		}
		for _, e := range listing.Entries {
			seen[e.Path]++
		}
		if listing.NextOffset == 0 {
			// Last page: should be the final partial page (25 % 10 == 5 entries).
			if want := total - offset; len(listing.Entries) != want {
				t.Fatalf("final page has %d entries, want %d", len(listing.Entries), want)
			}
			break
		}
		// NextOffset must advance past the entries just returned.
		if listing.NextOffset != offset+len(listing.Entries) {
			t.Fatalf("NextOffset = %d, want %d", listing.NextOffset, offset+len(listing.Entries))
		}
		offset = listing.NextOffset
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if want := (total + limit - 1) / limit; pages != want {
		t.Fatalf("paged in %d requests, want %d", pages, want)
	}
	if len(seen) != total {
		t.Fatalf("covered %d distinct entries, want %d", len(seen), total)
	}
	for p, count := range seen {
		if count != 1 {
			t.Fatalf("entry %q appeared %d times, want exactly once", p, count)
		}
	}

	// An offset at/past the total returns an empty page with no NextOffset.
	past, err := BrowseFS(root, rel, total+5, limit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(past.Entries) != 0 {
		t.Fatalf("offset past total returned %d entries, want 0", len(past.Entries))
	}
	if past.NextOffset != 0 {
		t.Fatalf("offset past total set NextOffset = %d, want 0", past.NextOffset)
	}

	// limit<=0 falls back to the 200 default, so all 25 entries fit on one page.
	def, err := BrowseFS(root, rel, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Entries) != total {
		t.Fatalf("limit<=0 returned %d entries, want all %d (default 200)", len(def.Entries), total)
	}
	if def.NextOffset != 0 {
		t.Fatalf("limit<=0 set NextOffset = %d, want 0 (everything on one page)", def.NextOffset)
	}
}

// TestBrowseFSAllowFilter verifies the allow predicate gates entries by their
// library-relative path AND that filtering happens before pagination - so a page
// stays full of permitted entries rather than being thinned by denied ones.
func TestBrowseFSAllowFilter(t *testing.T) {
	root := t.TempDir()
	const total = 20
	rel := writeAudioFiles(t, root, "Book", total) // track-0000.mp3 … track-0019.mp3

	// Permit only the even-indexed tracks (10 of the 20).
	allowed := map[string]bool{}
	allow := func(childRel string) bool {
		base := childRel[strings.LastIndexByte(childRel, '/')+1:]
		var idx int
		if _, err := fmt.Sscanf(base, "track-%04d.mp3", &idx); err != nil {
			return false
		}
		ok := idx%2 == 0
		if ok {
			allowed[childRel] = true
		}
		return ok
	}

	// One unpaged pass: only permitted entries appear, denied ones never leak.
	full, err := BrowseFS(root, rel, 0, 0, allow)
	if err != nil {
		t.Fatal(err)
	}
	if full.Total != total/2 {
		t.Fatalf("filtered Total = %d, want %d", full.Total, total/2)
	}
	for _, e := range full.Entries {
		var idx int
		if _, err := fmt.Sscanf(e.Name, "track-%04d.mp3", &idx); err != nil {
			t.Fatalf("unexpected entry name %q", e.Name)
		}
		if idx%2 != 0 {
			t.Fatalf("denied entry leaked through allow filter: %q", e.Name)
		}
	}

	// Filtering before pagination means a small limit yields full pages of
	// permitted entries (not a half-empty page from denied entries being dropped
	// after slicing). With 10 permitted entries and limit 5, the first page is a
	// full 5 and there is a second page.
	const limit = 5
	page0, err := BrowseFS(root, rel, 0, limit, allow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page0.Entries) != limit {
		t.Fatalf("first page has %d entries, want a full %d (filter must run before pagination)", len(page0.Entries), limit)
	}
	if page0.NextOffset != limit {
		t.Fatalf("first page NextOffset = %d, want %d", page0.NextOffset, limit)
	}
	page1, err := BrowseFS(root, rel, page0.NextOffset, limit, allow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Entries) != limit {
		t.Fatalf("second page has %d entries, want a full %d", len(page1.Entries), limit)
	}
	if page1.NextOffset != 0 {
		t.Fatalf("second page NextOffset = %d, want 0 (all permitted entries consumed)", page1.NextOffset)
	}
}
