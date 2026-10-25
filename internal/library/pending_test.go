package library

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

func entryNames(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// TestBrowseFSNaturalOrderAndShadowedMKA (#177): /fs lists folders first, then
// files, each in natural order, so the files list in the order the book plays
// them (1, 2, 10); and it leaves out the .mka a book of the folder leaves out
// (beside a file of its name in another format), keeping a lone one. ListDir
// (the next book's listing) has the same entries.
func TestBrowseFSNaturalOrderAndShadowedMKA(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, f := range []string{
		"Book/1 - a.mp3", "Book/2 - b.mp3", "Book/10 - c.mp3", "Book/Book.m4b", "Book/Book.mka", "Book/Solo.mka",
		"Book/.hidden.mp3", "Book/cover.jpg",
	} {
		writeFile(t, root, f, []byte("x"))
	}
	for _, d := range []string{"Book/Disc 10", "Book/Disc 2", "Book/disc 1"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"disc 1", "Disc 2", "Disc 10", "1 - a.mp3", "2 - b.mp3", "10 - c.mp3", "Book.m4b", "Solo.mka"}
	listing, err := BrowseFS(root, "Book", 0, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNames(listing.Entries); !slices.Equal(got, want) {
		t.Fatalf("BrowseFS = %q, want %q", got, want)
	}
	all, err := ListDir(root, "Book", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNames(all); !slices.Equal(got, want) {
		t.Fatalf("ListDir = %q, want %q", got, want)
	}

	// An ignored m4b shadows nothing: the scanner doesn't see it, so the .mka is
	// the book's file and is listed.
	ignored, err := BrowseFS(root, "Book", 0, 100, nil, ParseIgnore([]string{"Book.m4b"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNames(ignored.Entries); !slices.Contains(got, "Book.mka") || slices.Contains(got, "Book.m4b") {
		t.Fatalf("with Book.m4b ignored: %q, want Book.mka listed in its place", got)
	}
}

// TestMarkPendingBooks (#193): a folder the scanner would read as a book that
// the index doesn't hold is pending_book, by the scanner's own rules; anything
// else is not (a navigation folder, artwork, a collection, a disc of a joined
// book, a folder the caller reaches only as the way down, a folder holding an
// indexed book).
func TestMarkPendingBooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, f := range []string{
		"A/New Book/01.mp3",           // audio of its own: a book
		"A/Artwork/cover.jpg",         // no audio: a plain folder
		"A/Series/Book 1/01.mp3",      // only subfolders with audio: a navigation folder
		"A/Loose/one.mp3",             // a collection: a book per file, the folder none
		"A/Loose/two.mp3",             //
		"A/Discs/CD1/01.mp3",          // a disc set without an override: each disc a book
		"A/Discs/CD2/01.mp3",          //
		"A/Joined/CD1/01.mp3",         // a disc set with a book override: the folder the book
		"A/Joined/CD2/01.mp3",         //
		"A/Forced/Sub/01.mp3",         // a book override on a folder that is no disc set
		"A/Hidden/.secret.mp3",        // a hidden file is no audio
		"A/Ignored/01.mp3",            // an ignored folder is not listed
		"A/IgnoredFile/skip.mp3",      // an ignored file is no audio
		"A/Indexed Below/Book/01.mp3", // a book indexed below
		"A/Scoped/01.mp3",             // reached only on the way down
		"A/Scoped/Granted/01.mp3",     //
	} {
		writeFile(t, root, f, []byte("x"))
	}
	lib := catalog.Library{ID: 1, Name: "L", Root: root, IgnorePatterns: []string{"/A/Ignored/", "skip.mp3"}}
	ignore := ParseIgnore(lib.IgnorePatterns)
	overrides := map[string]string{
		"A/Loose":  catalog.OverrideCollection,
		"A/Joined": catalog.OverrideBook,
		"A/Forced": catalog.OverrideBook,
	}
	pending := func(rel string, holding map[string]bool, allow func(string) bool) map[string]bool {
		t.Helper()
		listing, err := BrowseFS(root, rel, 0, 200, nil, ignore)
		if err != nil {
			t.Fatal(err)
		}
		MarkPendingBooks(t.Context(), lib, listing.Entries, overrides, holding, allow, ignore)
		out := map[string]bool{}
		for _, e := range listing.Entries {
			if e.PendingBook {
				if e.IsBook || !e.IsDir {
					t.Errorf("%s: pending_book on a book or a file", e.Path)
				}
				out[e.Path] = true
			}
		}
		return out
	}
	holding := map[string]bool{"A/Indexed Below": true}
	got := pending("A", holding, nil)
	want := []string{"A/Joined", "A/New Book", "A/Scoped"}
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
		t.Fatalf("pending in A = %q, want %q", keys, want)
	}
	// Discs without an override are each a book; the folder holding them is not.
	if keys := slices.Sorted(maps.Keys(pending("A/Discs", nil, nil))); !slices.Equal(keys, []string{"A/Discs/CD1", "A/Discs/CD2"}) {
		t.Errorf("pending in A/Discs = %q, want both discs", keys)
	}
	// The discs of a joined folder are the joined book's, never books of their own.
	if keys := pending("A/Joined", nil, nil); len(keys) != 0 {
		t.Errorf("pending in A/Joined = %v, want none", keys)
	}
	// A folder that is a book below a folder with a book override (no disc set):
	// its own audio makes it a book, as the scan reads it.
	if keys := slices.Sorted(maps.Keys(pending("A/Forced", nil, nil))); !slices.Equal(keys, []string{"A/Forced/Sub"}) {
		t.Errorf("pending in A/Forced = %q", keys)
	}

	// Denied: a scope granting only A/Scoped/Granted sees A/Scoped on the way down
	// (VisibleInBrowse) but may not open it, so it is no pending book of theirs;
	// allowed: the granted folder is.
	allow := func(p string) bool { return p == "A/Scoped/Granted" || strings.HasPrefix(p, "A/Scoped/Granted/") }
	if got := pending("A", holding, allow); len(got) != 0 {
		t.Errorf("scoped caller in A: %v, want nothing pending", got)
	}
	if got := pending("A/Scoped", nil, allow); !got["A/Scoped/Granted"] {
		t.Errorf("scoped caller in A/Scoped: %v, want the granted folder pending", got)
	}

	// A folder holding an indexed book is not read from disk at all: one made
	// unreadable reads the same.
	if err := os.Chmod(filepath.Join(root, "A", "Indexed Below"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "A", "Indexed Below"), 0o755) })
	if got := pending("A", holding, nil); got["A/Indexed Below"] {
		t.Error("a folder holding an indexed book was marked")
	}
	if got := pending("A", nil, nil); got["A/Indexed Below"] {
		t.Error("a folder with only subfolders was marked")
	}
}
