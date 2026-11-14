package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// fetchJob is a done librivox.fetch of project 192 into libID as recording
// (its params), asked for by admin by (0 for none), its result naming path in
// resultLib.
func fetchJob(libID, resultLib int64, path string, by int64, recording string) bridge.Job {
	params, _ := json.Marshal(bridge.LibrivoxFetchParams{LibrivoxID: 192, WorkID: "johnny-crows-garden", RecordingID: recording, LibraryID: libID})
	j := bridge.Job{ID: 41, Kind: bridge.KindLibrivoxFetch, LibraryID: &libID, State: bridge.StateDone, Params: params,
		Result: json.RawMessage(fmt.Sprintf(`{"library_id":%d,"path":%q,"librivox_id":192,"files":3,"bytes":1000}`, resultLib, path))}
	if by != 0 {
		j.RequestedBy = &bridge.Requester{ID: by, Username: "u"}
	}
	return j
}

// matchRow is the book_matches row at path: work, recording, how and set_by
// (nil for none); ok false when there is none.
func matchRow(t *testing.T, c *Catalog, libID int64, path string) (work, recording, how string, setBy any, offered string, ok bool) {
	t.Helper()
	err := c.db.QueryRowContext(context.Background(), `SELECT work_id, recording_id, how, set_by, offered_asin FROM book_matches
		 WHERE library_id = ? AND path = ?`, libID, path).Scan(&work, &recording, &how, &setBy, &offered)
	if err != nil {
		return "", "", "", nil, "", false
	}
	return work, recording, how, setBy, offered, true
}

// TestFoldLibrivoxFetch: a done fetch records its book as the project's
// recording (accepted, set by the requesting admin), replacing a stale row at
// that path (a deleted book's leftover, another work) and never touching
// another path's; a result that disagrees with the job, names no indexed book,
// or a job with no recording records nothing. Only a recorded match wakes the
// OnBookChange listeners (the rescan woke them before the match was there).
func TestFoldLibrivoxFetch(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	uid := seedUser(t, c, ctx)
	const placed, neighbour = "L. Leslie Brooke/Johnny Crow's Garden", "L. Leslie Brooke/Johnny Crow's New Garden"
	for _, p := range []string{placed, neighbour} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
	}
	// A stale row at the placed path (another work, an offered ASIN) and the
	// neighbour's own match.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, recording_id, how, run_id, set_by, set_at, offered_asin)
		VALUES(?, ?, 'old-work', 'old-rec', 'auto', 3, NULL, '2020-01-01T00:00:00Z', 'B0OFFERED1')`, lib.ID, placed); err != nil {
		t.Fatal(err)
	}
	anchorBook(t, c, ctx, lib.ID, neighbour, "neighbour-work")
	var heard atomic.Int32
	c.OnBookChange(func() { heard.Add(1) })

	// Refused: a result naming another library, another project, the root, a
	// path that is no indexed book; a job with no recording, or not done.
	refused := map[string]bridge.Job{
		"another library": fetchJob(lib.ID, other.ID, placed, uid, "lv192"),
		"the root":        fetchJob(lib.ID, lib.ID, ".", uid, "lv192"),
		"traversal":       fetchJob(lib.ID, lib.ID, "../x", uid, "lv192"),
		"not indexed":     fetchJob(lib.ID, lib.ID, "L. Leslie Brooke/Gone", uid, "lv192"),
	}
	project := fetchJob(lib.ID, lib.ID, placed, uid, "lv192")
	project.Result = json.RawMessage(fmt.Sprintf(`{"library_id":%d,"path":%q,"librivox_id":193}`, lib.ID, placed))
	refused["another project"] = project
	for name, j := range refused {
		if err := c.FoldLibrivoxFetch(ctx, j); !errors.Is(err, ErrFetchNotIndexed) {
			t.Errorf("%s: %v, want ErrFetchNotIndexed", name, err)
		}
	}
	running := fetchJob(lib.ID, lib.ID, placed, uid, "lv192")
	running.State = bridge.StateRunning
	for name, j := range map[string]bridge.Job{
		"no recording": fetchJob(lib.ID, lib.ID, placed, uid, ""),
		"not done":     running,
	} {
		if err := c.FoldLibrivoxFetch(ctx, j); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if w, r, how, _, _, _ := matchRow(t, c, lib.ID, placed); w != "old-work" || r != "old-rec" || how != MatchHowAuto {
		t.Fatalf("a refused fold wrote %s %s %s", w, r, how)
	}
	if n := heard.Load(); n != 0 {
		t.Fatalf("a fold that recorded nothing woke the listeners %d times", n)
	}

	if err := c.FoldLibrivoxFetch(ctx, fetchJob(lib.ID, lib.ID, placed, uid, "lv192")); err != nil {
		t.Fatal(err)
	}
	w, r, how, setBy, offered, ok := matchRow(t, c, lib.ID, placed)
	if !ok || w != "johnny-crows-garden" || r != "lv192" || how != MatchHowAccepted || setBy != uid || offered != "" {
		t.Fatalf("placed = %s %s %s %v %q %t", w, r, how, setBy, offered, ok)
	}
	if n := heard.Load(); n != 1 {
		t.Fatalf("a recorded match woke the listeners %d times, want once", n)
	}
	if w, r, _, _, _, _ := matchRow(t, c, lib.ID, neighbour); w != "neighbour-work" || r != "rec-1" {
		t.Fatalf("the neighbour's match changed: %s %s", w, r)
	}
	if m, err := c.BookMatchAt(ctx, lib.ID, placed); err != nil || m == nil || m.RecordingID != "lv192" {
		t.Fatalf("BookMatchAt = %+v, %v", m, err)
	}
	// A requester since deleted: still recorded, set by nobody.
	if err := c.FoldLibrivoxFetch(ctx, fetchJob(lib.ID, lib.ID, placed, 0, "lv192")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, setBy, _, _ := matchRow(t, c, lib.ID, placed); setBy != nil {
		t.Fatalf("set_by = %v, want none", setBy)
	}
}

// TestRecordingBooks: where a LibriVox recording is: indexed books whose match
// names that exact recording of that work. A work-level match (no recording),
// another recording, another work's recording of the same id, and a match
// whose book is gone from the index are not.
func TestRecordingBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/One", "A/Two", "A/WorkOnly", "A/OtherRec", "A/OtherWork"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ path, work, rec string }{
		{"A/One", "w", "lv192"}, {"A/Two", "w", "lv192"}, {"A/WorkOnly", "w", ""},
		{"A/OtherRec", "w", "lv193"}, {"A/OtherWork", "x", "lv192"}, {"A/Deleted", "w", "lv192"},
	} {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, recording_id, how, set_at)
			VALUES(?, ?, ?, ?, 'accepted', 't')`, lib.ID, row.path, row.work, row.rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.RecordingBooks(ctx, "w", "lv192")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Ref{lib.ID, "A/One"}) || got[1] != (Ref{lib.ID, "A/Two"}) {
		t.Fatalf("RecordingBooks = %+v", got)
	}
	for _, rec := range []string{"", "lv194"} {
		if got, err := c.RecordingBooks(ctx, "w", rec); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("RecordingBooks(%q) = %#v, %v, want none", rec, got, err)
		}
	}
	// The work's recordings in one read: each recording's books, a work-level
	// match under no recording.
	by, err := c.WorkRecordingBooks(ctx, "w")
	if err != nil || len(by) != 2 || len(by["lv192"]) != 2 || len(by["lv193"]) != 1 || by["lv193"][0] != (Ref{lib.ID, "A/OtherRec"}) {
		t.Fatalf("WorkRecordingBooks = %+v, %v", by, err)
	}
	if by, err := c.WorkRecordingBooks(ctx, ""); err != nil || by == nil || len(by) != 0 {
		t.Fatalf("WorkRecordingBooks of no work = %#v, %v", by, err)
	}
}

// adoptedFetch is fetchJob with the result's adopted field: "" leaves it out
// (an older Bridge), else "true" or "false".
func adoptedFetch(libID int64, path string, by int64, adopted string) bridge.Job {
	j := fetchJob(libID, libID, path, by, "lv192")
	if adopted != "" {
		j.Result = json.RawMessage(fmt.Sprintf(`{"library_id":%d,"path":%q,"librivox_id":192,"files":3,"bytes":1000,"adopted":%s}`,
			libID, path, adopted))
	}
	return j
}

// TestFoldLibrivoxFetchAdopted (server#379): how the fold treats what is
// already at the book's path, by the result's adopted. A newly placed book
// (false) drops a deleted book's leftover ASIN and ISBN (its overrides,
// whoever set them, and its enrichment), so /meta no longer asks them before
// the match, and keeps every other edit; an adopted folder (true) keeps the
// match recorded there (an admin's rematch) and its identifiers, and is
// recorded when it has none (a fold that was lost); an older Bridge's result
// (no adopted) replaces the match and touches nothing else, as before.
func TestFoldLibrivoxFetchAdopted(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const path = "L. Leslie Brooke/Johnny Crow's Garden"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, path)); err != nil {
		t.Fatal(err)
	}
	// What a deleted book left at the path: an edited ASIN, a community ISBN,
	// an enrichment row and an edited title.
	leftovers := func(t *testing.T) {
		t.Helper()
		for _, q := range []string{
			`DELETE FROM book_overrides`, `DELETE FROM book_enrichment`, `DELETE FROM book_matches`,
			`INSERT INTO book_overrides(library_id, path, field, value, source, updated_at) VALUES(?1, ?2, 'asin', 'B0LEFTOVER', 'edited', 't')`,
			`INSERT INTO book_overrides(library_id, path, field, value, source, updated_at) VALUES(?1, ?2, 'isbn', '9780000000002', 'community', 't')`,
			`INSERT INTO book_overrides(library_id, path, field, value, source, updated_at) VALUES(?1, ?2, 'title', 'Edited Title', 'edited', 't')`,
			`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, set_by, set_at)
			 VALUES(?1, ?2, 'rematched-work', 'rematched-rec', 'accepted', NULL, 't')`,
		} {
			if _, err := c.db.ExecContext(ctx, q, lib.ID, path); err != nil {
				t.Fatal(err)
			}
		}
		setEnrichment(t, c, lib.ID, path, "B0ENRICHED", "") // refreshes the book's effective metadata
		if b := mustBook(t, c, ctx, lib.ID, path); b.ASIN != "B0LEFTOVER" || b.ISBN != "9780000000002" {
			t.Fatalf("leftovers not in effect: %q %q", b.ASIN, b.ISBN)
		}
	}
	overrides := func() map[string]storedOverride {
		o, err := queryOverrides(ctx, c.db, lib.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	enriched := func() int {
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_enrichment WHERE library_id = ? AND path = ?`, lib.ID, path).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("placed", func(t *testing.T) {
		leftovers(t)
		if err := c.FoldLibrivoxFetch(ctx, adoptedFetch(lib.ID, path, uid, "false")); err != nil {
			t.Fatal(err)
		}
		if w, r, _, _, _, _ := matchRow(t, c, lib.ID, path); w != "johnny-crows-garden" || r != "lv192" {
			t.Fatalf("match = %s %s, want the recording", w, r)
		}
		if o := overrides(); len(o) != 1 || o["title"].Value != "Edited Title" {
			t.Fatalf("overrides = %v, want only the title", o)
		}
		if n := enriched(); n != 0 {
			t.Fatalf("enrichment rows = %d, want none", n)
		}
		if b := mustBook(t, c, ctx, lib.ID, path); b.ASIN != "" || b.ISBN != "" || b.Title != "Edited Title" {
			t.Fatalf("book = %q %q %q, want no identifiers and the title kept", b.ASIN, b.ISBN, b.Title)
		}
	})
	t.Run("placed over a leftover enrichment alone", func(t *testing.T) {
		leftovers(t)
		if _, err := c.db.ExecContext(ctx, `DELETE FROM book_overrides WHERE field IN ('asin', 'isbn')`); err != nil {
			t.Fatal(err)
		}
		b := mustBook(t, c, ctx, lib.ID, path)
		if err := c.db.WithTx(ctx, "test", func(tx *sql.Tx) error { return refreshEffective(ctx, tx, b.ID) }); err != nil {
			t.Fatal(err)
		}
		if b := mustBook(t, c, ctx, lib.ID, path); b.ASIN != "B0ENRICHED" {
			t.Fatalf("the enrichment's ASIN is not in effect: %q", b.ASIN)
		}
		if err := c.FoldLibrivoxFetch(ctx, adoptedFetch(lib.ID, path, uid, "false")); err != nil {
			t.Fatal(err)
		}
		if b := mustBook(t, c, ctx, lib.ID, path); b.ASIN != "" || enriched() != 0 {
			t.Fatalf("ASIN %q, enrichment rows %d: the leftover enrichment stayed", b.ASIN, enriched())
		}
	})
	t.Run("adopted keeps a rematch", func(t *testing.T) {
		leftovers(t)
		if err := c.FoldLibrivoxFetch(ctx, adoptedFetch(lib.ID, path, uid, "true")); err != nil {
			t.Fatal(err)
		}
		if w, r, _, _, _, _ := matchRow(t, c, lib.ID, path); w != "rematched-work" || r != "rematched-rec" {
			t.Fatalf("match = %s %s, want the admin's rematch", w, r)
		}
		if o := overrides(); len(o) != 3 || enriched() != 1 {
			t.Fatalf("overrides = %v, enrichment %d: an adopted folder's were touched", o, enriched())
		}
	})
	t.Run("adopted with no match is recorded", func(t *testing.T) {
		leftovers(t)
		if _, err := c.db.ExecContext(ctx, `DELETE FROM book_matches`); err != nil {
			t.Fatal(err)
		}
		if err := c.FoldLibrivoxFetch(ctx, adoptedFetch(lib.ID, path, uid, "true")); err != nil {
			t.Fatal(err)
		}
		if w, r, how, setBy, _, _ := matchRow(t, c, lib.ID, path); w != "johnny-crows-garden" || r != "lv192" ||
			how != MatchHowAccepted || setBy != uid {
			t.Fatalf("match = %s %s %s %v, want the recording", w, r, how, setBy)
		}
		if o := overrides(); len(o) != 3 {
			t.Fatalf("overrides = %v: an adopted folder's were touched", o)
		}
	})
	t.Run("an older Bridge", func(t *testing.T) {
		leftovers(t)
		if err := c.FoldLibrivoxFetch(ctx, adoptedFetch(lib.ID, path, uid, "")); err != nil {
			t.Fatal(err)
		}
		if w, r, _, _, _, _ := matchRow(t, c, lib.ID, path); w != "johnny-crows-garden" || r != "lv192" {
			t.Fatalf("match = %s %s, want the recording (replaced, as before)", w, r)
		}
		if o := overrides(); len(o) != 3 || enriched() != 1 {
			t.Fatalf("overrides = %v, enrichment %d: an older Bridge's fold touched them", o, enriched())
		}
	})
}
