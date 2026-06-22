package catalog

import "testing"

func TestNormCollapsesPunctuationAndCase(t *testing.T) {
	cases := map[string]string{
		"The  Hobbit!!":   "the hobbit",
		"Tolkien, J.R.R.": "tolkien j r r",
		"  Dune  ":        "dune",
		"":                "",
		"!!!":             "",
		"Æon":             "æon",
	}
	for in, want := range cases {
		if got := norm(in); got != want {
			t.Errorf("norm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExposedDedupKeyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		book Book
		want string
	}{
		{"asin wins", Book{ASIN: "B01", ISBN: "9", Title: "X", Author: "Y", ContentHash: "h"}, "a:b01"},
		{"isbn next", Book{ISBN: "9-78", Title: "X", Author: "Y", ContentHash: "h"}, "i:9 78"},
		{"metadata next", Book{Title: "The Hobbit", Author: "Tolkien", ContentHash: "h"}, "m:tolkien|the hobbit|"},
		{"narrator in key", Book{Title: "X", Author: "Y", Narrator: "Z"}, "m:y|x|z"},
		{"hash last resort", Book{ContentHash: "abc"}, "h:abc"},
		{"nothing to match", Book{}, ""},
	}
	for _, tc := range cases {
		if got := exposedDedupKey(tc.book); got != tc.want {
			t.Errorf("%s: exposedDedupKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBetterThanQualityPrecedence(t *testing.T) {
	mk := func(format string, files int, size int64, dur float64, sortOrder, rank int) candidate {
		return candidate{
			book:      Book{Format: format, Size: size, Duration: dur},
			fileCount: files, sortOrder: sortOrder, rankIdx: rank,
		}
	}
	// Format tier: m4b beats a (larger, single) mp3.
	if m4b, mp3 := mk("m4b", 1, 100, 100, 9, 9), mk("mp3", 1, 999, 100, 0, 0); !m4b.betterThan(mp3) {
		t.Error("m4b should beat mp3 on format tier")
	}
	// Single beats multipart within the same format.
	if single, multi := mk("m4b", 1, 100, 100, 9, 9), mk("m4b", 5, 100, 100, 0, 0); !single.betterThan(multi) {
		t.Error("single-file should beat multipart at equal format")
	}
	// Bitrate breaks ties when format + structure match.
	if big, small := mk("m4b", 1, 200, 100, 9, 9), mk("m4b", 1, 100, 100, 0, 0); !big.betterThan(small) {
		t.Error("higher bitrate should win at equal format + structure")
	}
	// Otherwise-identical copies fall back to library sort order.
	if first, second := mk("m4b", 1, 100, 100, 0, 9), mk("m4b", 1, 100, 100, 1, 0); !first.betterThan(second) {
		t.Error("lower sort_order should win for identical copies")
	}
}

func TestDedupBooksGroupsAndPicksWinner(t *testing.T) {
	mk := func(lib int64, libName, path, title, author, format, hash string, files int, size int64, sortOrder, rank int) candidate {
		return candidate{
			book: Book{LibraryID: lib, RelPath: path, Title: title, Author: author,
				Format: format, ContentHash: hash, Size: size, Duration: 100},
			libName: libName, fileCount: files, sortOrder: sortOrder, rankIdx: rank,
		}
	}

	// Two copies of the same book (matched by metadata) + one distinct book. The
	// distinct one comes first by rank, then the duplicate group.
	cands := []candidate{
		mk(1, "A", "dune.m4b", "Dune", "Herbert", "m4b", "", 1, 100, 0, 0),
		mk(1, "A", "hobbit.m4b", "The Hobbit", "Tolkien", "m4b", "", 1, 100, 0, 1),
		mk(2, "B", "hobbit/", "The Hobbit", "Tolkien", "mp3", "", 3, 100, 1, 2),
	}
	out := dedupBooks(cands, 0)
	if len(out) != 2 {
		t.Fatalf("expected 2 groups, got %d: %+v", len(out), out)
	}
	if out[0].Title != "Dune" || out[1].Title != "The Hobbit" {
		t.Fatalf("group order not preserved by rank: %q, %q", out[0].Title, out[1].Title)
	}
	// The Hobbit winner is the single m4b (lib A), with the mp3 as another location.
	win := out[1]
	if win.LibraryID != 1 {
		t.Fatalf("expected the m4b copy (lib 1) to win, got lib %d", win.LibraryID)
	}
	if win.MultiFile == nil || *win.MultiFile {
		t.Fatalf("winner should be single-file, got %+v", win.MultiFile)
	}
	if win.DedupKey != "m:tolkien|the hobbit|" {
		t.Fatalf("unexpected dedup_key %q", win.DedupKey)
	}
	if len(win.OtherLocations) != 1 || win.OtherLocations[0].LibraryID != 2 || !win.OtherLocations[0].MultiFile {
		t.Fatalf("unexpected other_locations: %+v", win.OtherLocations)
	}

	// Content-hash unions two copies even when their metadata differs.
	hashed := []candidate{
		mk(1, "A", "x.m4b", "Mistagged", "", "m4b", "deadbeef", 1, 100, 0, 0),
		mk(2, "B", "y.m4b", "The Real Title", "Author", "m4b", "deadbeef", 1, 100, 1, 1),
	}
	if out := dedupBooks(hashed, 0); len(out) != 1 {
		t.Fatalf("content-hash should union differing metadata into 1 group, got %d", len(out))
	}

	// limit caps the number of groups returned.
	if out := dedupBooks(cands, 1); len(out) != 1 || out[0].Title != "Dune" {
		t.Fatalf("limit not honored: %+v", out)
	}
}

func TestDedupGenericTitlesAndDistinctLocations(t *testing.T) {
	mk := func(lib int64, libName, title, author, hash string, sortOrder, rank int) candidate {
		return candidate{
			book: Book{LibraryID: lib, RelPath: title + "-" + hash + ".mp3", Title: title, Author: author,
				Format: "mp3", ContentHash: hash, Size: 100, Duration: 100},
			libName: libName, fileCount: 1, sortOrder: sortOrder, rankIdx: rank,
		}
	}
	// Different books sharing a generic title + author must NOT merge — only their
	// (differing) content fingerprints decide, and those differ.
	generic := []candidate{
		mk(1, "A", "Track 01", "Cressida Cowell", "hashA", 0, 0),
		mk(2, "B", "Track 01", "Cressida Cowell", "hashB", 1, 1),
	}
	if out := dedupBooks(generic, 0); len(out) != 2 {
		t.Fatalf("generic titles with different content must not merge, got %d groups", len(out))
	}
	// Same generic title with the SAME fingerprint (true copies) still merge.
	copies := []candidate{
		mk(1, "A", "Track 01", "Cressida Cowell", "same", 0, 0),
		mk(2, "B", "Track 01", "Cressida Cowell", "same", 1, 1),
	}
	if out := dedupBooks(copies, 0); len(out) != 1 {
		t.Fatalf("identical-fingerprint copies should merge, got %d groups", len(out))
	}

	// other_locations: one entry per OTHER library even when a library holds several
	// copies, and never the winner's own library.
	multi := []candidate{
		mk(1, "A", "The Hobbit", "Tolkien", "h", 0, 0), // winner (lib A)
		mk(1, "A", "The Hobbit", "Tolkien", "h", 0, 1), // 2nd copy in lib A — must not appear
		mk(2, "B", "The Hobbit", "Tolkien", "h", 1, 2), // lib B copy
		mk(2, "B", "The Hobbit", "Tolkien", "h", 1, 3), // 2nd copy in lib B — collapse to one
	}
	out := dedupBooks(multi, 0)
	if len(out) != 1 {
		t.Fatalf("expected 1 group, got %d", len(out))
	}
	if locs := out[0].OtherLocations; len(locs) != 1 || locs[0].LibraryID != 2 {
		t.Fatalf("expected one other-location (lib B only), got %+v", locs)
	}
}

func TestSearchDeduplicatesAcrossLibraries(t *testing.T) {
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/b"})
	book := func(lib int64, path string) *Book {
		return &Book{LibraryID: lib, RelPath: path, Title: "The Hobbit", Author: "Tolkien", Format: "m4b", Size: 1000, Duration: 100}
	}
	if _, err := c.UpsertBook(ctx, book(libA.ID, "Tolkien/Hobbit.m4b")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, book(libB.ID, "audiobooks/hobbit.m4b")); err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, AllowAll: true}}

	// libA ordered first → its copy wins, libB is "also on".
	if err := c.ReorderLibraries(ctx, []int64{libA.ID, libB.ID}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Search(ctx, "hobbit", scopes, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 deduped result, got %d: %+v", len(res), res)
	}
	if res[0].LibraryID != libA.ID || res[0].DedupKey == "" {
		t.Fatalf("expected libA winner with dedup_key, got %+v", res[0])
	}
	if len(res[0].OtherLocations) != 1 || res[0].OtherLocations[0].LibraryID != libB.ID {
		t.Fatalf("expected other_locations=[libB], got %+v", res[0].OtherLocations)
	}

	// Reordering flips which copy wins.
	if err := c.ReorderLibraries(ctx, []int64{libB.ID, libA.ID}); err != nil {
		t.Fatal(err)
	}
	if res, _ := c.Search(ctx, "hobbit", scopes, 10); len(res) != 1 || res[0].LibraryID != libB.ID {
		t.Fatalf("expected libB winner after reorder, got %+v", res)
	}

	// Scope safety: a user who can only see libA gets libA's copy and no leaked
	// pointer to libB.
	onlyA := []Scope{{LibraryID: libA.ID, AllowAll: true}}
	if res, _ := c.Search(ctx, "hobbit", onlyA, 10); len(res) != 1 || res[0].LibraryID != libA.ID || len(res[0].OtherLocations) != 0 {
		t.Fatalf("scoped search leaked or wrong: %+v", res)
	}
}

func TestSearchDedupPrefersQualityOverOrder(t *testing.T) {
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/b"})
	if err := c.ReorderLibraries(ctx, []int64{libA.ID, libB.ID}); err != nil { // libA ranked first
		t.Fatal(err)
	}
	// libA: multipart mp3 (ranked first but lower quality). libB: single m4b.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "hobbit/", Title: "The Hobbit", Author: "Tolkien",
		Format: "mp3", Size: 500, Duration: 100,
		Files: []BookFile{{RelPath: "hobbit/01.mp3", Seq: 1}, {RelPath: "hobbit/02.mp3", Seq: 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: libB.ID, RelPath: "hobbit.m4b", Title: "The Hobbit", Author: "Tolkien",
		Format: "m4b", Size: 900, Duration: 100}); err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, AllowAll: true}}
	res, err := c.Search(ctx, "hobbit", scopes, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(res), res)
	}
	if res[0].LibraryID != libB.ID {
		t.Fatalf("expected the m4b (libB) to win on quality despite libA ranked first, got lib %d", res[0].LibraryID)
	}
	if res[0].MultiFile == nil || *res[0].MultiFile {
		t.Fatalf("winner should be single-file, got %+v", res[0].MultiFile)
	}
}

func TestRecentBooksDeduplicates(t *testing.T) {
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/b"})
	if err := c.ReorderLibraries(ctx, []int64{libA.ID, libB.ID}); err != nil {
		t.Fatal(err)
	}
	dup := func(lib int64, path, added string) *Book {
		return &Book{LibraryID: lib, RelPath: path, Title: "Dup", Author: "X", Format: "m4b", Size: 1000, Duration: 100, AddedAt: added}
	}
	c.UpsertBook(ctx, dup(libA.ID, "a.m4b", "2024-01-01T00:00:00Z"))
	c.UpsertBook(ctx, dup(libB.ID, "b.m4b", "2024-02-01T00:00:00Z"))
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "u.m4b", Title: "Unique", Author: "Y", Format: "m4b", Size: 1, Duration: 1, AddedAt: "2024-03-01T00:00:00Z"})

	scopes := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, AllowAll: true}}
	res, err := c.RecentBooks(ctx, scopes, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 results (one deduped + one unique), got %d: %+v", len(res), res)
	}
	if res[0].Title != "Unique" || res[1].Title != "Dup" {
		t.Fatalf("unexpected order: %q, %q", res[0].Title, res[1].Title)
	}
	if res[1].LibraryID != libA.ID || len(res[1].OtherLocations) != 1 {
		t.Fatalf("dup winner should be libA with 1 other location, got %+v", res[1])
	}
}
