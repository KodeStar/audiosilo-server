package catalog

import (
	"slices"
	"testing"
)

// TestUnknownASINCandidates: the report's candidate rule, book by book.
func TestUnknownASINCandidates(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	books := []struct {
		lib                 int64
		path, asin, isbn    string
		tagASIN             string
		override, overValue string // a book_overrides asin row's source and value ("" for none)
	}{
		{lib.ID, "own", " b0own00001 ", "", "", "", ""},                        // its own ASIN: in, upper-cased and trimmed
		{lib.ID, "own-twice", "B0OWN00001", "", "", "", ""},                    // the same ASIN again: once
		{lib.ID, "community", "B0COMM0001", "", "", "community", "B0COMM0001"}, // a community match's: out
		{lib.ID, "edited", "B0EDIT0001", "", "", "edited", "B0EDIT0001"},       // an admin's own edit: in
		{lib.ID, "tag", "", "", "B0TAG00001", "", ""},                          // a tag on a book with no identifier: in
		{lib.ID, "tag-isbn", "", "9780000000001", "B0TAG00002", "", ""},        // the book has an ISBN: out
		{lib.ID, "tag-own", "B0OWN00003", "", "B0TAG00003", "", ""},            // the book has an ASIN: its tag out
		{lib.ID, "tag-cleared", "", "", "B0TAG00004", "edited", ""},            // an admin cleared its ASIN: out
		{lib.ID, "batch-1", "", "", "B0BATCH001", "", ""},                      // a tag on two books: out
		{lib.ID, "batch-2", "", "", "b0batch001", "", ""},
		{lib.ID, "bad", "NOTANASIN", "", "", "", ""},    // not an ASIN's form: out
		{pods.ID, "show", "B0POD00001", "", "", "", ""}, // a podcast library: out
	}
	for _, b := range books {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: b.lib, RelPath: b.path, Title: b.path, Author: "A",
			ASIN: b.asin, ISBN: b.isbn, TagASIN: b.tagASIN, AddedAt: "2020-01-01"}); err != nil {
			t.Fatal(err)
		}
		if b.override != "" {
			if _, err := c.db.ExecContext(ctx, `INSERT INTO book_overrides(library_id, path, field, value, source, updated_at)
				VALUES(?, ?, 'asin', ?, ?, 't')`, b.lib, b.path, b.overValue, b.override); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := c.UnknownASINCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"B0EDIT0001", "B0OWN00001", "B0OWN00003", "B0TAG00001"}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// The report's memory: a check, a send, a recheck that finds it known, a drop.
func TestUnknownASINRows(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	if at, err := c.LastASINSentAt(ctx); err != nil || at != "" {
		t.Fatalf("last sent before any = %q, %v", at, err)
	}
	if err := c.SaveASINChecks(ctx, []ASINCheck{{ASIN: "B0UNKNOWN1", Unknown: true}, {ASIN: "B0KNOWN001"}}, "2026-11-01T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkASINsSent(ctx, []string{"B0UNKNOWN1"}, "2026-11-01T00:00:01.000Z"); err != nil {
		t.Fatal(err)
	}
	// Still unknown a month on: the send is remembered.
	if err := c.SaveASINChecks(ctx, []ASINCheck{{ASIN: "B0UNKNOWN1", Unknown: true}}, "2026-12-02T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	rows, err := c.UnknownASINRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rows["B0UNKNOWN1"]; !r.Unknown || r.SentAt != "2026-11-01T00:00:01.000Z" || r.CheckedAt != "2026-12-02T00:00:00.000Z" {
		t.Fatalf("unknown row = %+v", r)
	}
	if r := rows["B0KNOWN001"]; r.Unknown || r.SentAt != "" {
		t.Fatalf("known row = %+v", r)
	}
	if at, err := c.LastASINSentAt(ctx); err != nil || at != "2026-11-01T00:00:01.000Z" {
		t.Fatalf("last sent = %q, %v", at, err)
	}
	// Now known: its send is forgotten.
	if err := c.SaveASINChecks(ctx, []ASINCheck{{ASIN: "B0UNKNOWN1"}}, "2026-12-03T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if err := c.DropUnknownASINs(ctx, []string{"B0KNOWN001"}); err != nil {
		t.Fatal(err)
	}
	rows, err = c.UnknownASINRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := rows["B0UNKNOWN1"]; len(rows) != 1 || !ok || r.Unknown || r.SentAt != "" {
		t.Fatalf("rows after the recheck and drop = %+v", rows)
	}

	st, err := c.UnknownASINReportState(ctx)
	if err != nil || st != (UnknownASINReport{}) {
		t.Fatalf("state before any = %+v, %v", st, err)
	}
	want := UnknownASINReport{State: "ok", LastRunAt: "2026-12-03T00:00:00.000Z", LastSent: 3}
	if err := c.SaveUnknownASINReportState(ctx, want); err != nil {
		t.Fatal(err)
	}
	if st, err := c.UnknownASINReportState(ctx); err != nil || st != want {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// TestASINChecksMoveTheContentGenerationOnAChangeOnly (server#370): Health's
// unknown_asin kind is cached under the content generation, so the report's own
// writers (an upsert, on a WITHOUT ROWID table) must move it when an ASIN is
// first looked up, changes its answer or is forgotten, and leave it for a
// lookup or a send that only renews the row's times.
func TestASINChecksMoveTheContentGenerationOnAChangeOnly(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	step := func(name string, moves bool, write func() error) {
		t.Helper()
		before, ok := c.ContentGeneration()
		if !ok {
			t.Fatal("the test store counts no content generation")
		}
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after, _ := c.ContentGeneration(); (after != before) != moves {
			t.Errorf("%s: the content generation moved = %v, want %v", name, after != before, moves)
		}
	}
	both := []ASINCheck{{ASIN: "B0UNKNOWN1", Unknown: true}, {ASIN: "B0KNOWN001"}}
	step("first lookup", true, func() error { return c.SaveASINChecks(ctx, both, "2026-11-14T00:00:00.000Z") })
	step("same answers", false, func() error { return c.SaveASINChecks(ctx, both, "2026-11-15T00:00:00.000Z") })
	step("sent", false, func() error {
		return c.MarkASINsSent(ctx, []string{"B0UNKNOWN1"}, "2026-11-15T00:00:01.000Z")
	})
	step("same answer once sent", false, func() error {
		return c.SaveASINChecks(ctx, both[:1], "2026-11-16T00:00:00.000Z")
	})
	step("now known", true, func() error {
		return c.SaveASINChecks(ctx, []ASINCheck{{ASIN: "B0UNKNOWN1"}}, "2026-11-17T00:00:00.000Z")
	})
	step("now unknown", true, func() error {
		return c.SaveASINChecks(ctx, []ASINCheck{{ASIN: "B0KNOWN001", Unknown: true}}, "2026-11-17T00:00:00.000Z")
	})
	step("forgotten", true, func() error { return c.DropUnknownASINs(ctx, []string{"B0KNOWN001"}) })
	step("nothing to forget", false, func() error { return c.DropUnknownASINs(ctx, []string{"B0NOTHELD1"}) })
}
