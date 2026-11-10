package catalog

import (
	"database/sql"
	"testing"
	"time"
)

// A book that moves takes its Audible titles only once its places have moved
// (server#303): the re-match trigger then offers the moved book's place to
// Audible, never the row an earlier book left at the new path, and the book's
// own state moving alone leaves the titles where they were.
func TestAudibleTitlesMoveAfterTheirPlaces(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	on := true
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Sync: &on}); err != nil {
		t.Fatal(err)
	}
	const oldPath, newPath = "Author/Old", "Author/New"
	title := avail("B000000001", "")
	title.Match = &AudibleMatch{LibraryID: f.lib.ID, Path: oldPath, How: "asin"}
	f.poll(t, "p1", title)
	save := func(path string, pos float64, at time.Time) {
		t.Helper()
		if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: Ref{LibraryID: f.lib.ID, Path: path}, Position: pos,
			Duration: 36000, DeviceID: "phone", UpdatedAt: at.UTC().Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	save(newPath, 100, f.clock.Add(-48*time.Hour)) // an earlier book's, left behind
	save(oldPath, 5000, f.clock.Add(-time.Hour))
	acct, err := f.c.AudibleSyncFor(f.ctx, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{{LibraryID: f.lib.ID, AllowAll: true}}
	rows, cursor, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, "", 0)
	if err != nil || len(rows) != 1 || rows[0].Position != 5000 {
		t.Fatalf("feed = %+v, %v", rows, err)
	}
	feed := func() []AudibleProgressRow {
		t.Helper()
		rows, _, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, cursor, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// The book's own state alone: the titles stay, so nothing is offered.
	if err := f.c.db.WithTx(f.ctx, "test", func(tx *sql.Tx) error {
		return moveBookState(f.ctx, tx, f.lib.ID, oldPath, newPath, f.clock)
	}); err != nil {
		t.Fatal(err)
	}
	if got := feed(); len(got) != 0 {
		t.Fatalf("feed after the book's state moved = %+v, want the left-behind row never offered", got)
	}
	var at string
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT match_path FROM audible_titles WHERE asin = ?`, title.ASIN).Scan(&at); err != nil || at != oldPath {
		t.Fatalf("title matched to %q, %v; want it left at %q", at, err, oldPath)
	}

	if err := f.c.MoveDurableState(f.ctx, f.lib.ID, oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if got := feed(); len(got) != 1 || got[0].Path != newPath || got[0].Position != 5000 {
		t.Fatalf("feed after the move = %+v, want the moved book's place", got)
	}
}
