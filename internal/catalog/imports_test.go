package catalog

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// importFixture is a sessionFixture with a second book and an import in review.
type importFixture struct {
	*sessionFixture
	other Ref
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	f := &importFixture{sessionFixture: newSessionFixture(t), other: Ref{LibraryID: 0, Path: "Other/Book"}}
	f.other.LibraryID = f.lib
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib, RelPath: f.other.Path, IsFolder: true,
		Title: "Other", Author: "Ann Author", Duration: 3600}); err != nil {
		t.Fatal(err)
	}
	return f
}

// newImport records an import for the fixture's user and moves it to review.
func (f *importFixture) newImport(t *testing.T, cutoff time.Time) int64 {
	t.Helper()
	imps, err := f.c.CreateImports(f.ctx, []Import{{UserID: f.user, Source: ImportSourceABS,
		SourceURL: "http://abs.local", SourceID: "abs-1", Cutoff: FormatCutoff(cutoff)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.FinishImportFetch(f.ctx, imps[0].ID, "alex", []byte("payload"), ImportPlan{}); err != nil {
		t.Fatal(err)
	}
	return imps[0].ID
}

// day is 2026-09-<d> at hour h, UTC.
func day(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }

// write is what the tests apply: three sessions on the book (one per day, an
// hour each from 20:00), an estimate, a bookmark, a new finished progress row on
// the book and an advanced one on other (prior given by the state).
func (f *importFixture) write(st ImportState) *ImportWrite {
	w := &ImportWrite{}
	for d := 1; d <= 3; d++ {
		w.Sessions = append(w.Sessions, ImportSession{Ref: f.book, DeviceName: "Apple iPhone 15", Platform: "ios",
			StartedAt: day(d, 20), LastAt: day(d, 21), StartPos: float64(d-1) * 2400, EndPos: float64(d) * 2400,
			Duration: 7200, Listened: 3600})
	}
	w.Days = []ImportDay{{Ref: f.book, Day: "2026-08-20", Listened: 900}}
	w.Bookmarks = []Bookmark{{Ref: f.book, Position: 1234, Note: "Imported", CreatedAt: FormatStamp(day(2, 20))}}
	w.Progress = []ImportProgress{{Wrote: Progress{Ref: f.book, Position: 7200, Duration: 7200, Finished: true,
		PlaybackSpeed: 1, Version: 1, UpdatedAt: FormatStamp(day(3, 21)), StartedAt: "2026-09-01T20:00:00Z",
		FinishedAt: "2026-09-03T21:00:00Z"}}}
	if have, ok := st.Progress[f.other]; ok {
		up := have
		up.Position, up.Version, up.UpdatedAt = 3000, have.Version+1, FormatStamp(day(3, 22))
		w.Progress = append(w.Progress, ImportProgress{Prior: &have, Wrote: up})
	}
	w.Plan.Summary.Sessions = len(w.Sessions)
	return w
}

func (f *importFixture) apply(t *testing.T, id int64) *Import {
	t.Helper()
	imp, err := f.c.ApplyImport(f.ctx, id, func(st ImportState) (*ImportWrite, error) { return f.write(st), nil })
	if err != nil {
		t.Fatal(err)
	}
	return imp
}

// pages lists every session f matches, a page of f.Limit at a time.
func (f *importFixture) pages(t *testing.T, flt SessionFilter) []Session {
	t.Helper()
	var all []Session
	for range 100 {
		page, next, err := f.c.ListSessions(f.ctx, flt)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == 0 {
			return all
		}
		flt.Before = next
	}
	t.Fatal("the pages never end")
	return nil
}

func starts(sessions []Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.StartedAt
	}
	return out
}

// importSessions applies an import for userID writing one hour-long session on
// the fixture's book at each start, and returns its id.
func (f *importFixture) importSessions(t *testing.T, userID int64, at ...time.Time) int64 {
	t.Helper()
	imps, err := f.c.CreateImports(f.ctx, []Import{{UserID: userID, Source: ImportSourceABS, SourceURL: "http://abs"}})
	if err != nil {
		t.Fatal(err)
	}
	id := imps[0].ID
	if err := f.c.FinishImportFetch(f.ctx, id, "abs", []byte("payload"), ImportPlan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ApplyImport(f.ctx, id, func(ImportState) (*ImportWrite, error) {
		w := &ImportWrite{}
		for _, start := range at {
			w.Sessions = append(w.Sessions, ImportSession{Ref: f.book, StartedAt: start,
				LastAt: start.Add(time.Hour), Duration: 7200, Listened: 3600})
		}
		return w, nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *importFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.c.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestApplyAndUndoImport: an apply writes the sessions (listed by when they
// started, so under the live one), the estimate, the bookmark and the progress;
// an undo takes them all back out, restoring progress nobody changed since.
func TestApplyAndUndoImport(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.listen(t, 7, f.book, 100) // a live session, on 1 October
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 600, Duration: 3600,
		UpdatedAt: day(2, 12).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	id := f.newImport(t, time.Time{})
	imp := f.apply(t, id)
	if imp.Status != ImportApplied || imp.AppliedAt == nil || imp.Summary.Sessions != 3 {
		t.Fatalf("applied = %+v", imp)
	}
	if p, err := f.c.ImportPayload(f.ctx, id); err != nil || p != nil {
		t.Errorf("the payload outlived the apply: %d bytes, %v", len(p), err)
	}

	// Imported sessions have newer ids but older starts: they list under the live
	// one, newest first.
	sessions := f.pages(t, SessionFilter{UserID: f.user, Limit: 2})
	if got := starts(sessions); len(got) != 4 || got[0] != FormatStamp(f.clock.Add(-15*time.Second)) ||
		got[1] != FormatStamp(day(3, 20)) || got[3] != FormatStamp(day(1, 20)) {
		t.Fatalf("sessions newest first = %v", got)
	}
	if s := sessions[0]; s.Imported || s.Backfilled {
		t.Errorf("live session = %+v", s)
	}
	if s := sessions[3]; !s.Backfilled || !s.Imported || s.Client == nil || s.Client.App != ImportClientApp ||
		s.DeviceName != "Apple iPhone 15" || s.StartedAt != "2026-09-01T20:00:00.000Z" || s.ID <= sessions[0].ID {
		t.Errorf("oldest imported session = %+v", s)
	}

	// Each imported session is a history span too, which the book's History and
	// the Journal (/me/history) list.
	spans, err := f.c.ListHistory(f.ctx, f.user, f.book, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 || spans[0].From != 4800 || spans[0].To != 7200 ||
		spans[0].StartedAt != "2026-09-03T20:00:00.000Z" || spans[0].EndedAt != "2026-09-03T21:00:00.000Z" {
		t.Errorf("book history = %+v", spans)
	}
	all, err := f.c.ListAllHistory(f.ctx, f.user, []Scope{{LibraryID: f.lib, AllowAll: true}}, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.History) != 3 || all.History[2].StartedAt != "2026-09-01T20:00:00.000Z" || all.History[0].Book == nil {
		t.Errorf("all history = %+v", all.History)
	}

	// The person's stats count it: three hours on three days, the estimate in
	// the totals only, the book finished on 3 September.
	scopes, _ := f.c.UserScopes(f.ctx, f.user, true)
	st, err := f.c.UserStatsFor(f.ctx, "2026", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC, f.user, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if st.Totals.Listened != 3*3600+900+15 || st.Estimated != 900 || st.Totals.Finished != 1 ||
		len(st.FinishedBooks) != 1 || st.FinishedBooks[0].FinishedAt != "2026-09-03T21:00:00Z" {
		t.Errorf("stats totals = %+v estimated %v finished %+v", st.Totals, st.Estimated, st.FinishedBooks)
	}
	var days int
	for _, d := range st.Days {
		if d.Listened > 0 {
			days++
		}
	}
	if days != 4 || len(st.TopBooks) == 0 || st.TopBooks[0].Path != f.book.Path || st.HourWeekday[1][20] != 3600 {
		t.Errorf("days %d, top books %+v, Tue 20:00 %v", days, st.TopBooks, st.HourWeekday[1][20])
	}

	// The listener moves on with "other" after the import: theirs stays on undo.
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 3100, Duration: 3600,
		UpdatedAt: day(4, 12).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	undone, err := f.c.UndoImport(f.ctx, id)
	if err != nil || undone.Status != ImportUndone {
		t.Fatalf("undo = %+v, %v", undone, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE import_id <> 0`) +
		f.count(t, `SELECT COUNT(*) FROM listening_daily WHERE import_id <> 0`) +
		f.count(t, `SELECT COUNT(*) FROM bookmarks WHERE import_id <> 0`) +
		f.count(t, `SELECT COUNT(*) FROM listening_history WHERE import_id <> 0`) +
		f.count(t, `SELECT COUNT(*) FROM import_progress_prior`); n != 0 {
		t.Errorf("%d imported rows left after the undo", n)
	}
	if spans, _ := f.c.ListHistory(f.ctx, f.user, f.book, 0); len(spans) != 0 {
		t.Errorf("history after the undo = %+v", spans)
	}
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.book); p != nil {
		t.Errorf("the progress the import created survived the undo: %+v", p)
	}
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.other); p == nil || p.Position != 3100 {
		t.Errorf("the listener's own progress = %+v", p)
	}
	if _, err := f.c.UndoImport(f.ctx, id); !errors.Is(err, ErrImportNotApplied) {
		t.Errorf("second undo: %v", err)
	}
}

// TestUndoRestoresUnchangedProgress: a row the import advanced and nobody
// touched since goes back to what it was, under a newer version and stamp.
func TestUndoRestoresUnchangedProgress(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 600, Duration: 3600,
		UpdatedAt: day(2, 12).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	id := f.newImport(t, time.Time{})
	f.apply(t, id)
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.other); p.Position != 3000 {
		t.Fatalf("imported position = %v", p.Position)
	}
	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	p, _ := f.c.GetProgress(f.ctx, f.user, f.other)
	if p.Position != 600 || p.Version != 3 || p.UpdatedAt != FormatStamp(f.clock) {
		t.Errorf("restored = %+v", p)
	}
}

// TestReimportReplaces: applying a second import takes the first back out in
// the same transaction, so nothing is counted twice.
func TestReimportReplaces(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	first := f.newImport(t, time.Time{})
	f.apply(t, first)
	second := f.newImport(t, time.Time{})

	// The review of the second reads the state as the first's undo leaves it.
	st, err := f.c.ImportStateFor(f.ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Progress[f.book]; ok || len(st.Bookmarks[f.book]) != 0 {
		t.Errorf("review state still holds the first import: %+v %+v", st.Progress, st.Bookmarks)
	}

	f.apply(t, second)
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions`); n != 3 {
		t.Errorf("%d sessions after a re-import, want 3", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_history WHERE import_id = ?`, second); n != 3 ||
		f.count(t, `SELECT COUNT(*) FROM listening_history`) != 3 {
		t.Errorf("%d history spans of the re-import (want 3, and no others)", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM bookmarks`); n != 1 {
		t.Errorf("%d bookmarks after a re-import, want 1", n)
	}
	d, _ := f.c.GetImport(f.ctx, first)
	if d.Status != ImportUndone {
		t.Errorf("first import = %s, want undone", d.Status)
	}
	if err := f.c.DeleteImport(f.ctx, second); !errors.Is(err, ErrImportApplied) {
		t.Errorf("delete applied: %v", err)
	}
	if err := f.c.DeleteImport(f.ctx, first); err != nil {
		t.Errorf("delete undone: %v", err)
	}
}

// TestReimportRestoresProgressExactly: a re-import's undo of the previous import
// puts the person's row back exactly as it was (its own updated_at, version
// bumped), the same row the review planned against, so the new import moves it
// on again; a row the new import leaves where the old one had moved it is
// stamped now, as a standalone undo would.
func TestReimportRestoresProgressExactly(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	own := day(2, 12).Format(time.RFC3339)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 600, Duration: 3600,
		UpdatedAt: own}); err != nil {
		t.Fatal(err)
	}
	first := f.newImport(t, time.Time{})
	f.apply(t, first)
	moved, _ := f.c.GetProgress(f.ctx, f.user, f.other)
	if moved.Position != 3000 {
		t.Fatalf("first import = %+v", moved)
	}

	// The review of the re-import sees the row as the undo will put it back.
	second := f.newImport(t, time.Time{})
	st, err := f.c.ImportStateFor(f.ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := st.Progress[f.other]
	if reviewed.Position != 600 || reviewed.UpdatedAt != own || reviewed.Version != moved.Version+1 {
		t.Fatalf("review state = %+v", reviewed)
	}
	var applied ImportState
	if _, err := f.c.ApplyImport(f.ctx, second, func(st ImportState) (*ImportWrite, error) {
		applied = st
		return f.write(st), nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := applied.Progress[f.other]; got != reviewed {
		t.Errorf("apply planned against %+v, the review against %+v", got, reviewed)
	}
	p, _ := f.c.GetProgress(f.ctx, f.user, f.other)
	if p.Position != 3000 || p.UpdatedAt != moved.UpdatedAt || p.Version != moved.Version+2 {
		t.Errorf("re-imported = %+v (first import wrote %+v)", p, moved)
	}

	// A third import that doesn't move it: back to the person's position, stamped
	// now so a device holding the imported row takes it.
	third := f.newImport(t, time.Time{})
	if _, err := f.c.ApplyImport(f.ctx, third, func(ImportState) (*ImportWrite, error) {
		return &ImportWrite{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = f.c.GetProgress(f.ctx, f.user, f.other)
	if p.Position != 600 || p.UpdatedAt != FormatStamp(f.clock) {
		t.Errorf("left behind = %+v", p)
	}
}

// TestUndoKeepsEditedBookmarks: an undo (and a re-import's) deletes only the
// imported bookmarks still as the import wrote them; one the person relabelled
// or re-noted is theirs and stays, and the re-import's review counts it.
func TestUndoKeepsEditedBookmarks(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	marks := func(ImportState) (*ImportWrite, error) {
		w := &ImportWrite{}
		for i, note := range []string{"Kept as is", "Relabelled", "Renoted"} {
			w.Bookmarks = append(w.Bookmarks, Bookmark{Ref: f.book, Position: float64(100 * (i + 1)), Note: note,
				CreatedAt: FormatStamp(day(2, 20))})
		}
		return w, nil
	}
	first := f.newImport(t, time.Time{})
	if _, err := f.c.ApplyImport(f.ctx, first, marks); err != nil {
		t.Fatal(err)
	}
	have, err := f.c.ListBookmarks(f.ctx, f.user, f.book)
	if err != nil || len(have) != 3 {
		t.Fatalf("bookmarks = %+v, %v", have, err)
	}
	scopes := []Scope{{LibraryID: f.lib, AllowAll: true}}
	label, note := "favourite", "My own words"
	for _, e := range []struct {
		id   int64
		edit BookmarkEdit
	}{{have[1].ID, BookmarkEdit{Label: &label}}, {have[2].ID, BookmarkEdit{Note: &note}}} {
		if _, err := f.c.EditBookmark(f.ctx, f.user, e.id, e.edit, scopes); err != nil {
			t.Fatal(err)
		}
	}

	// The re-import's review already leaves out only the untouched one.
	second := f.newImport(t, time.Time{})
	st, err := f.c.ImportStateFor(f.ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Bookmarks[f.book]; len(got) != 2 || got[0].Note != "Relabelled" || got[1].Note != note {
		t.Errorf("review bookmarks = %+v", got)
	}
	if _, err := f.c.ApplyImport(f.ctx, second, func(ImportState) (*ImportWrite, error) { return &ImportWrite{}, nil }); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM bookmarks WHERE import_id = 0`); n != 2 ||
		f.count(t, `SELECT COUNT(*) FROM bookmarks`) != 2 {
		t.Errorf("%d bookmarks handed to the person, want the 2 edited (and nothing else)", n)
	}

	// A standalone undo does the same.
	third := f.newImport(t, time.Time{})
	if _, err := f.c.ApplyImport(f.ctx, third, marks); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UndoImport(f.ctx, second); !errors.Is(err, ErrImportNotApplied) {
		t.Fatalf("undo of the replaced import: %v", err)
	}
	have, _ = f.c.ListBookmarks(f.ctx, f.user, f.book)
	var thirds []int64
	for _, b := range have {
		if b.Note == "Kept as is" {
			thirds = append(thirds, b.ID)
		}
	}
	if len(thirds) != 1 {
		t.Fatalf("bookmarks = %+v", have)
	}
	if _, err := f.c.EditBookmark(f.ctx, f.user, thirds[0], BookmarkEdit{Label: &label}, scopes); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UndoImport(f.ctx, third); err != nil {
		t.Fatal(err)
	}
	have, _ = f.c.ListBookmarks(f.ctx, f.user, f.book)
	if len(have) != 3 || f.count(t, `SELECT COUNT(*) FROM bookmarks WHERE import_id <> 0`) != 0 {
		t.Errorf("after the undo = %+v", have)
	}
}

// TestPruneKeepsImportID: imported sessions past the retention roll up with
// their import's id, so the undo still removes them.
func TestPruneKeepsImportID(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.listen(t, 7, f.book, 100)
	id := f.newImport(t, time.Time{})
	f.apply(t, id)
	if _, err := f.c.PruneSessions(f.ctx, day(15, 0), time.UTC); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_daily WHERE import_id = ? AND estimated = 0`, id); n != 3 {
		t.Errorf("%d rolled-up imported days, want 3", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions`); n != 1 {
		t.Errorf("%d sessions left, want the live one", n)
	}
	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_daily`); n != 0 {
		t.Errorf("%d daily rows left after the undo", n)
	}
}

// TestApplyRollsUpOldSessions: an imported session already past the session
// retention is written as the day total PruneSessions would make of it (its day
// in the server's zone, its import's id), the rest as sessions; the history has
// a span for each, the stats are the same as with every session raw, and an
// undo takes both back out.
func TestApplyRollsUpOldSessions(t *testing.T) {
	t.Parallel()
	east := time.FixedZone("east", 5*3600) // 20:00 UTC is 01:00 the next day
	stats := func(f *importFixture) UserStats {
		t.Helper()
		scopes, _ := f.c.UserScopes(f.ctx, f.user, true)
		st, err := f.c.UserStatsFor(f.ctx, "2026", time.Date(2026, 1, 1, 0, 0, 0, 0, east),
			time.Date(2027, 1, 1, 0, 0, 0, 0, east), east, f.user, scopes)
		if err != nil {
			t.Fatal(err)
		}
		return *st
	}
	ref := newImportFixture(t)
	ref.apply(t, ref.newImport(t, time.Time{}))
	want := stats(ref)

	f := newImportFixture(t)
	id := f.newImport(t, time.Time{})
	// The cutoff is the second session's last save: only the first is before it.
	if _, err := f.c.ApplyImport(f.ctx, id, func(st ImportState) (*ImportWrite, error) {
		w := f.write(st)
		w.RollUpBefore, w.Zone = day(2, 21), east
		return w, nil
	}); err != nil {
		t.Fatal(err)
	}
	sessions := f.pages(t, SessionFilter{UserID: f.user})
	if got := starts(sessions); len(got) != 2 || got[1] != FormatStamp(day(2, 20)) {
		t.Errorf("sessions = %v", got)
	}
	var (
		date     string
		listened float64
		count    int
	)
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT day, listened, sessions FROM listening_daily
	     WHERE import_id = ? AND estimated = 0`, id).Scan(&date, &listened, &count); err != nil {
		t.Fatal(err)
	}
	if date != "2026-09-02" || listened != 3600 || count != 1 {
		t.Errorf("rolled-up day = %s %v %d", date, listened, count)
	}
	if spans, _ := f.c.ListHistory(f.ctx, f.user, f.book, 0); len(spans) != 3 {
		t.Errorf("history spans = %d, want one per imported session", len(spans))
	}
	got := stats(f)
	if got.Totals != want.Totals || got.Estimated != want.Estimated || !slices.Equal(got.Days, want.Days) ||
		len(got.TopBooks) != len(want.TopBooks) || got.TopBooks[0].Listened != want.TopBooks[0].Listened {
		t.Errorf("stats = %+v %v, want %+v %v", got.Totals, got.TopBooks, want.Totals, want.TopBooks)
	}

	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions`) +
		f.count(t, `SELECT COUNT(*) FROM listening_daily`) +
		f.count(t, `SELECT COUNT(*) FROM listening_history`); n != 0 {
		t.Errorf("%d imported rows left after the undo", n)
	}
}

// TestCutoffOffset: an import carries the server's offset at its cutoff (that
// day's, daylight saving included), so the console reads the server's day; none
// without a cutoff.
func TestCutoffOffset(t *testing.T) {
	t.Parallel()
	if cutoffOffset(time.Time{}, time.UTC) != nil {
		t.Error("an offset without a cutoff")
	}
	if london, err := time.LoadLocation("Europe/London"); err == nil {
		winter, summer := time.Date(2026, 1, 10, 0, 0, 0, 0, london), time.Date(2026, 7, 10, 0, 0, 0, 0, london)
		if w, s := cutoffOffset(winter, london), cutoffOffset(summer, london); *w != 0 || *s != 60 {
			t.Errorf("London offsets = %d, %d", *w, *s)
		}
	}
	f := newImportFixture(t)
	d, err := f.c.GetImport(f.ctx, f.newImport(t, day(1, 0)))
	_, want := day(1, 0).In(time.Local).Zone()
	if err != nil || d.CutoffOffset == nil || *d.CutoffOffset != want/60 {
		t.Errorf("import = %+v, %v", d, err)
	}
	if d, _ := f.c.GetImport(f.ctx, f.newImport(t, time.Time{})); d.CutoffOffset != nil {
		t.Errorf("an import without a cutoff has an offset: %d", *d.CutoffOffset)
	}
}

// TestMoveCarriesImport: a moved book takes its imported sessions with it, and
// the undo still finds the progress it wrote at the new path.
func TestMoveCarriesImport(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 600, Duration: 3600,
		UpdatedAt: day(2, 12).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	id := f.newImport(t, time.Time{})
	f.apply(t, id)
	if err := f.c.MoveDurableState(f.ctx, f.lib, f.other.Path, "Moved/Book"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.MoveDurableState(f.ctx, f.lib, f.book.Path, "Moved/Main"); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE rel_path = 'Moved/Main' AND import_id = ?`, id); n != 3 {
		t.Errorf("%d imported sessions moved, want 3", n)
	}
	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	moved := Ref{LibraryID: f.lib, Path: "Moved/Book"}
	if p, _ := f.c.GetProgress(f.ctx, f.user, moved); p == nil || p.Position != 600 {
		t.Errorf("restored at the new path = %+v", p)
	}
}

func TestListeningStart(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	loc := time.FixedZone("east", 2*3600)
	if got, err := f.c.ListeningStart(f.ctx, f.user, loc); err != nil || !got.IsZero() {
		t.Fatalf("no listening: %v, %v", got, err)
	}
	f.apply(t, f.newImport(t, time.Time{})) // imported listening doesn't count
	if got, _ := f.c.ListeningStart(f.ctx, f.user, loc); !got.IsZero() {
		t.Errorf("imported listening counted: %v", got)
	}
	f.listen(t, 7, f.book, 100)
	if got, _ := f.c.ListeningStart(f.ctx, f.user, loc); !got.Equal(f.clock.Add(-15 * time.Second)) {
		t.Errorf("first session start = %v", got)
	}
	if _, err := f.c.db.ExecContext(f.ctx, `INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions)
		VALUES('2026-06-01', ?, ?, ?, 60, 1)`, f.user, f.lib, f.book.Path); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)
	if got, _ := f.c.ListeningStart(f.ctx, f.user, loc); !got.Equal(want) {
		t.Errorf("first rolled-up day = %v, want %v", got, want)
	}
	if c := FormatCutoff(want); *c != "2026-05-31T22:00:00Z" {
		t.Errorf("cutoff = %s", *c)
	}
}

// TestImportStatuses: one import in flight per user; a stopped server's fetch
// fails as interrupted and its apply goes back to review; steps out of turn are
// refused.
func TestImportStatuses(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	imps, err := f.c.CreateImports(f.ctx, []Import{{UserID: f.user, Source: ImportSourceABS, SourceURL: "http://x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.CreateImports(f.ctx, []Import{{UserID: f.user, Source: ImportSourceABS, SourceURL: "http://x"}}); !errors.Is(err, ErrImportRunning) {
		t.Errorf("second import while fetching: %v", err)
	}
	if _, err := f.c.ApplyImport(f.ctx, imps[0].ID, nil); !errors.Is(err, ErrImportNotReady) {
		t.Errorf("apply while fetching: %v", err)
	}
	if err := f.c.SetImportReview(f.ctx, imps[0].ID, nil, ImportPlan{}); !errors.Is(err, ErrImportNotReady) {
		t.Errorf("review while fetching: %v", err)
	}
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE imports SET status = ? WHERE id = ?`, ImportReview, imps[0].ID); err != nil {
		t.Fatal(err)
	}
	other, err := f.c.CreateImports(f.ctx, []Import{{UserID: f.user, Source: ImportSourceABS, SourceURL: "http://x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE imports SET status = ? WHERE id = ?`, ImportApplying, imps[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.c.InterruptImports(f.ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := f.c.GetImport(f.ctx, imps[0].ID)
	b, _ := f.c.GetImport(f.ctx, other[0].ID)
	if a.Status != ImportReview || b.Status != ImportFailed || b.ErrorCode != ImportInterrupted || b.Error == "" {
		t.Errorf("after interrupt: %+v / %+v", a.Import, b.Import)
	}
	// A failed build leaves the import in review, nothing written.
	if _, err := f.c.ApplyImport(f.ctx, a.ID, func(ImportState) (*ImportWrite, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Fatal("apply with a failing build succeeded")
	}
	if a, _ = f.c.GetImport(f.ctx, a.ID); a.Status != ImportReview {
		t.Errorf("after a failed apply: %s", a.Status)
	}
	if _, err := f.c.GetImport(f.ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing import: %v", err)
	}
	list, err := f.c.ListImports(f.ctx, f.user)
	if err != nil || len(list) != 2 || list[0].ID != other[0].ID || list[0].Username != "u" {
		t.Errorf("list = %+v, %v", list, err)
	}
}

// TestSessionsOrderByStart: the session lists go by when a session started, not
// by id: imported sessions (new ids) newer than live ones list above them, and
// two imports applied in either order interleave by time, page by page.
func TestSessionsOrderByStart(t *testing.T) {
	t.Parallel()
	for _, bFirst := range []bool{false, true} {
		f := newImportFixture(t)
		f.clock = day(1, 9)
		f.listen(t, 7, f.book, 0) // live, on 1 September: older than every import
		other := seedUserNamed(t, f.c, f.ctx, "other")
		a := func() { f.importSessions(t, f.user, day(2, 10), day(4, 10), day(6, 10)) }
		b := func() { f.importSessions(t, other, day(3, 10), day(5, 10), day(7, 10)) }
		if bFirst {
			b()
			a()
		} else {
			a()
			b()
		}
		f.clock = day(30, 9)
		got := starts(f.pages(t, SessionFilter{Limit: 2}))
		want := []string{FormatStamp(day(7, 10)), FormatStamp(day(6, 10)), FormatStamp(day(5, 10)),
			FormatStamp(day(4, 10)), FormatStamp(day(3, 10)), FormatStamp(day(2, 10)), FormatStamp(day(1, 9))}
		if !slices.Equal(got, want) {
			t.Errorf("b first %v: everyone's sessions = %v, want %v", bFirst, got, want)
		}
		mine := starts(f.pages(t, SessionFilter{UserID: f.user, Limit: 1}))
		if !slices.Equal(mine, []string{want[1], want[3], want[5], want[6]}) {
			t.Errorf("b first %v: one person's = %v", bFirst, mine)
		}
		book := starts(f.pages(t, SessionFilter{LibraryID: f.lib, Path: f.book.Path, Limit: 3}))
		if !slices.Equal(book, want) {
			t.Errorf("b first %v: one book's = %v", bFirst, book)
		}
	}
}

// TestSessionsCursorGone: a page's cursor session deleted before the next page
// is read (retention, an undo) neither skips nor repeats a session.
func TestSessionsCursorGone(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.importSessions(t, f.user, day(1, 10), day(2, 10)) // older than every live session
	for i := range 5 {
		f.clock = day(10+i, 9)
		f.listen(t, int64(10+i), f.book, 0)
	}
	del := func(id int64) {
		t.Helper()
		if _, err := f.c.db.ExecContext(f.ctx, `DELETE FROM listening_sessions WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	read := func(before int64, limit int) ([]Session, int64) {
		t.Helper()
		page, next, err := f.c.ListSessions(f.ctx, SessionFilter{UserID: f.user, Before: before, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		return page, next
	}
	all, _ := read(0, 100)
	if len(all) != 7 {
		t.Fatalf("%d sessions", len(all))
	}

	// A live cursor goes: the page continues with the next live session.
	_, next := read(0, 2)
	del(next)
	page, next := read(next, 3)
	if len(page) != 3 || page[0].ID != all[2].ID || page[2].ID != all[4].ID {
		t.Errorf("after a gone cursor: %v, want %v", starts(page), starts(all[2:5]))
	}

	// The oldest live session is the cursor and goes: only imported sessions are
	// left, and none is skipped.
	del(next)
	rest, next := read(next, 10)
	if next != 0 || len(rest) != 2 || !rest[0].Imported || rest[0].ID != all[5].ID || rest[1].ID != all[6].ID {
		t.Errorf("after the oldest live cursor went: %v", starts(rest))
	}

	// With before_at the page continues in place whatever went: an imported
	// cursor that goes (an undo, a roll-up) restarts nothing.
	gone := rest[0]
	del(gone.ID)
	at, err := time.Parse(time.RFC3339Nano, gone.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	tail, tnext, err := f.c.ListSessions(f.ctx, SessionFilter{UserID: f.user, Before: gone.ID, BeforeAt: at, Limit: 10})
	if err != nil || tnext != 0 || len(tail) != 1 || tail[0].ID != all[6].ID {
		t.Errorf("after a gone imported cursor with before_at: %v (%v)", starts(tail), err)
	}
}

// TestLiveSessionsLeaveImports: an imported session whose last save is recent
// (an import with no cutoff) is never live.
func TestLiveSessionsLeaveImports(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.importSessions(t, f.user, f.clock.Add(-30*time.Minute)) // last save 30 minutes on: now
	if live, err := f.c.LiveSessions(f.ctx); err != nil || len(live) != 0 {
		t.Errorf("live = %+v, %v", live, err)
	}
	f.listen(t, 7, f.book, 0)
	if live, _ := f.c.LiveSessions(f.ctx); len(live) != 1 || live[0].Imported {
		t.Errorf("live = %+v", live)
	}
}

// TestCreateImportsReturnsAll: one start for several people answers every
// import it recorded, in order.
func TestCreateImportsReturnsAll(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	other := seedUserNamed(t, f.c, f.ctx, "other")
	imps, err := f.c.CreateImports(f.ctx, []Import{
		{UserID: f.user, Source: ImportSourceABS, SourceURL: "http://abs", SourceID: "a"},
		{UserID: other, Source: ImportSourceABS, SourceURL: "http://abs", SourceID: "b"},
	})
	if err != nil || len(imps) != 2 || imps[0].UserID != f.user || imps[1].Username != "other" ||
		imps[1].SourceID != "b" || imps[0].Status != ImportFetching || imps[0].CreatedAt == "" {
		t.Fatalf("created = %+v, %v", imps, err)
	}
	// The payload is kept apart, and goes with the import.
	if err := f.c.FinishImportFetch(f.ctx, imps[0].ID, "a", []byte("payload"), ImportPlan{}); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.c.ImportPayload(f.ctx, imps[0].ID); string(p) != "payload" {
		t.Errorf("payload = %q", p)
	}
	if err := f.c.DeleteImport(f.ctx, imps[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM import_payloads`); n != 0 {
		t.Errorf("%d payloads left", n)
	}
}
