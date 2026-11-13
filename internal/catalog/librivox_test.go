package catalog

import (
	"context"
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
}
