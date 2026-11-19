package library

import (
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// server#359: a path longer than any route takes (only a Windows library can
// hold one) is not indexed, since every route would refuse it: the scan skips
// the first folder or audio file on the way down to go past the cap, logs it
// once as unreadable, and isn't partial for it (the rest is pruned as usual).
// A book's own read of its folder (audioEntries) leaves the file out too.
// Not parallel: it lowers the cap, as no disk the tests run on holds a path
// past the real one.
func TestScanSkipsOverLongPaths(t *testing.T) {
	maxIndexedPath = 20
	t.Cleanup(func() { maxIndexedPath = catalog.MaxPathBytes })
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	audio := testAudio(t)
	for _, p := range []string{
		"Short/Book/01.m4b",                    // indexed
		"A folder past the cap/Book/01.m4b",    // the folder is over: skipped with all below it
		"Mid/Book/01.m4b",                      // indexed
		"Mid/Book/02 - a long name.m4b",        // the file is over: left out of its book
		".a hidden folder past it/Book/01.m4b", // hidden: skipped as ever, not logged
		"Ignored past the cap too/Book/01.m4b", // ignored: skipped as ever, not logged
	} {
		writeFile(t, root, p, audio)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Long", Root: root, IgnorePatterns: []string{"Ignored past the cap too/"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewScanner(cat, "", nil).Scan(ctx, *lib)
	if err != nil || res.Partial {
		t.Fatalf("scan: partial %v, err %v", res.Partial, err)
	}
	if res.Books != 2 || res.Added != 2 {
		t.Fatalf("scan found %d books, added %d; want Short/Book and Mid/Book", res.Books, res.Added)
	}
	for _, p := range []string{"Short/Book", "Mid/Book"} {
		b, err := cat.GetBookByPath(ctx, lib.ID, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(b.Files) > 1 {
			t.Fatalf("%s holds %d files, want the long-named one left out", p, len(b.Files))
		}
	}
	var logged []string
	for _, e := range res.Log {
		if e.Kind == "unreadable" {
			logged = append(logged, e.Path)
			if e.Level != "warn" || e.Detail != errPathTooLong.Error() {
				t.Errorf("event %+v, want a warning with the path cap's reason", e)
			}
		}
	}
	slices.Sort(logged)
	if want := []string{"A folder past the cap", "Mid/Book/02 - a long name.m4b"}; !slices.Equal(logged, want) {
		t.Fatalf("logged %q, want %q", logged, want)
	}
	files, err := audioEntries(ctx, root, absOf(*lib, "Mid/Book"), nil, 0)
	if err != nil || len(files) != 1 || relPathOf(root, files[0].abs) != "Mid/Book/01.m4b" {
		t.Fatalf("audioEntries(Mid/Book) = %v, %v; want only 01.m4b", files, err)
	}
}
