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
	const oldPath, newPath = "Author/Old", "Author/New"
	f := syncedTitleAt(t, oldPath)
	f.savePlace(t, newPath, 100, f.clock.Add(-48*time.Hour)) // an earlier book's, left behind
	f.savePlace(t, oldPath, 5000, f.clock.Add(-time.Hour))
	acct, err := f.c.AudibleSyncFor(f.ctx, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{{LibraryID: f.lib.ID, AllowAll: true}}
	rows, cursor, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, "", 0, true)
	if err != nil || len(rows) != 1 || rows[0].Position != 5000 {
		t.Fatalf("feed = %+v, %v", rows, err)
	}
	feed := func() []AudibleProgressRow {
		t.Helper()
		rows, _, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, cursor, 0, true)
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
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT match_path FROM audible_titles WHERE asin = ?`, syncedASIN).Scan(&at); err != nil || at != oldPath {
		t.Fatalf("title matched to %q, %v; want it left at %q", at, err, oldPath)
	}

	if err := f.c.MoveDurableState(f.ctx, f.lib.ID, oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if got := feed(); len(got) != 1 || got[0].Path != newPath || got[0].Position != 5000 {
		t.Fatalf("feed after the move = %+v, want the moved book's place", got)
	}
}

// server#359: a row left at the new path that the move doesn't change is
// another book's place, never offered to Audible under the moved book's
// title: its person never played the moved book, or the left row was the
// newer and won the merge. Written after the feed's last row (the person's
// newest write, as a book just listened to and then replaced is), it is not
// read by that feed's cursor, nor by a feed that starts afresh. Its next write
// offers it. A row the move does carry is offered (above).
func TestAudibleMoveLeavesALeftRowUnoffered(t *testing.T) {
	t.Parallel()
	const oldPath, newPath = "Author/Old", "Author/New"
	for _, tc := range []struct {
		name   string
		played bool // the person has a place on the moved book (older than the left row)
	}{{"never played", false}, {"left row newer", true}} {
		f := syncedTitleAt(t, oldPath)
		if tc.played {
			f.savePlace(t, oldPath, 5000, f.clock.Add(-48*time.Hour))
		}
		acct, err := f.c.AudibleSyncFor(f.ctx, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		scopes := []Scope{{LibraryID: f.lib.ID, AllowAll: true}}
		feed := func(since string) ([]AudibleProgressRow, string) {
			t.Helper()
			rows, next, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, since, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			return rows, next
		}
		_, cursor := feed("")                                 // the Bridge's last sync, before the left row was written
		f.savePlace(t, newPath, 100, f.clock.Add(-time.Hour)) // an earlier book's, left behind
		if err := f.c.MoveDurableState(f.ctx, f.lib.ID, oldPath, newPath); err != nil {
			t.Fatal(err)
		}
		if p, _ := f.c.GetProgress(f.ctx, f.user, Ref{LibraryID: f.lib.ID, Path: newPath}); p == nil || p.Position != 100 {
			t.Fatalf("%s: row at the new path = %+v, want the left row", tc.name, p)
		}
		for _, since := range []string{cursor, ""} {
			if got, _ := feed(since); len(got) != 0 {
				t.Fatalf("%s: feed from %q after the move = %+v, want the left row never offered", tc.name, since, got)
			}
		}
		// Its next write offers it, as any write does.
		f.savePlace(t, newPath, 160, f.clock)
		if got, _ := feed(cursor); len(got) != 1 || got[0].Position != 160 {
			t.Fatalf("%s: feed after the row's next write = %+v, want it offered", tc.name, got)
		}
	}
}

// server#359: a row already on a join's folder is the joined book's own place
// (a folder joined, split and joined again finds the one it left), so a title
// matched to a part offers it to Audible once the title moves onto the folder,
// whether the person never played the part or the part's place is behind it:
// only a move takes a left row's offer back.
func TestAudibleJoinOffersTheFoldersOwnRow(t *testing.T) {
	t.Parallel()
	const into, part = "Author/Book", "Author/Book/Disc 1"
	for _, tc := range []struct {
		name   string
		played bool // the person has a place on the part, older and behind
	}{{"never played the part", false}, {"the part's place behind", true}} {
		f := syncedTitleAt(t, part)
		acct, err := f.c.AudibleSyncFor(f.ctx, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		scopes := []Scope{{LibraryID: f.lib.ID, AllowAll: true}}
		if tc.played {
			f.savePlace(t, part, 300, f.clock.Add(-48*time.Hour))
		}
		_, cursor, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, "", 0, true)
		if err != nil {
			t.Fatal(err)
		}
		f.savePlace(t, into, 18000, f.clock.Add(-time.Hour)) // from when the folder was joined before
		if err := f.c.JoinDurableState(f.ctx, f.lib.ID, into, []JoinPart{{Path: part, Last: true}}, 0); err != nil {
			t.Fatal(err)
		}
		for _, since := range []string{cursor, ""} {
			rows, _, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, since, 0, true)
			if err != nil || len(rows) != 1 || rows[0].Path != into || rows[0].Position != 18000 || rows[0].ASIN != syncedASIN {
				t.Fatalf("%s: feed from %q after the join = %+v, %v; want the folder's own place", tc.name, since, rows, err)
			}
		}
	}
}

// syncedASIN is syncedTitleAt's title.
const syncedASIN = "B000000001"

// syncedTitleAt is an Audible fixture whose account syncs places, with one
// title (syncedASIN) matched to the book at path.
func syncedTitleAt(t *testing.T, path string) *audibleFixture {
	t.Helper()
	f := newAudibleFixture(t, "manual")
	on := true
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Sync: &on}); err != nil {
		t.Fatal(err)
	}
	title := avail(syncedASIN, "")
	title.Match = &AudibleMatch{LibraryID: f.lib.ID, Path: path, How: "asin"}
	f.poll(t, "p1", title)
	return f
}

// savePlace saves the fixture user's place on the book at path, from the
// phone, at at.
func (f *audibleFixture) savePlace(t *testing.T, path string, pos float64, at time.Time) {
	t.Helper()
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: Ref{LibraryID: f.lib.ID, Path: path}, Position: pos,
		Duration: 36000, DeviceID: "phone", UpdatedAt: FormatStamp(at)}); err != nil {
		t.Fatal(err)
	}
}
