package catalog

import "testing"

// A book indexed by today's revision of the length reading is never stale; one
// indexed before it (duration_rev 0, as migration 0082 leaves every book) is
// stale when it holds an MP3, its own format or a part's, so the next scan
// reads it once more (SRV-16); a book with no MP3 is not.
func TestSignaturesDurationStale(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, b := range []*Book{
		{RelPath: "Loose.mp3", Format: "mp3"},
		{RelPath: "Loose.m4b", Format: "m4b"},
		{RelPath: "Mixed", IsFolder: true, Format: "m4b", Files: []BookFile{
			{RelPath: "Mixed/01.m4b", Seq: 0, Format: "m4b"}, {RelPath: "Mixed/02.mp3", Seq: 1, Format: "mp3"}}},
		{RelPath: "Parts", IsFolder: true, Format: "m4a", Files: []BookFile{
			{RelPath: "Parts/01.m4a", Seq: 0, Format: "m4a"}}},
	} {
		b.LibraryID, b.Title = lib.ID, b.RelPath
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	stale := func() map[string]bool {
		t.Helper()
		sigs, err := c.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for p, s := range sigs {
			out[p] = s.DurationStale
		}
		return out
	}
	for p, s := range stale() {
		if s {
			t.Errorf("%s: stale as indexed, want today's revision", p)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Loose.mp3": true, "Loose.m4b": false, "Mixed": true, "Parts": false}
	for p, s := range stale() {
		if s != want[p] {
			t.Errorf("%s: stale %t before the revision, want %t", p, s, want[p])
		}
	}
	// Indexed again, it is today's.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Loose.mp3", Title: "Loose", Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	if stale()["Loose.mp3"] {
		t.Error("Loose.mp3: still stale once indexed again")
	}
}
