package catalog

import (
	"testing"
	"time"
)

// server#303: a row names an Audible account only while it holds a place that
// account's own sync wrote (the guard, migration 0083). A writer that keeps or
// copies the account's device while giving the row a place (a new position, a
// finish, a joined part) leaves it with none, so GET /bridge/progress offers
// that place to Audible; a write that leaves the place as it was (a start date,
// an edit naming only dates, a move carrying the row) keeps the account's
// device, and the feed still leaves Audible's own place out.
func TestAudibleDeviceGuard(t *testing.T) {
	t.Parallel()
	const path, moved = "Author/Book", "Author/Moved"
	f := syncedTitleAt(t, path)
	ref := Ref{LibraryID: f.lib.ID, Path: path}
	f.savePlace(t, path, 1000, f.clock.Add(-time.Hour))
	acct, err := f.c.AudibleSyncFor(f.ctx, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	scopes := []Scope{{LibraryID: f.lib.ID, AllowAll: true}}
	feed := func() []AudibleProgressRow {
		t.Helper()
		rows, _, _, err := f.c.AudibleProgress(f.ctx, acct, scopes, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.c.db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// audibles makes the row at p Audible's place, as the Audible route leaves
	// it (ownAudiblePlace: the device alone, which the guard leaves).
	audibles := func(p string) {
		t.Helper()
		exec(`UPDATE progress SET device_id = 'audible:acct0001', device_name = 'Audible (US)'
		       WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.user, f.lib.ID, p)
	}
	device := func(p string) (string, string) {
		t.Helper()
		var id, name string
		if err := f.c.db.QueryRowContext(f.ctx, `SELECT device_id, device_name FROM progress
		    WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.user, f.lib.ID, p).Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		return id, name
	}
	wantAudible := func(p, what string) {
		t.Helper()
		if id, name := device(p); id != "audible:acct0001" || name != "Audible (US)" {
			t.Fatalf("%s: device %q %q, want Audible's kept", what, id, name)
		}
	}
	wantNone := func(p, what string) {
		t.Helper()
		if id, name := device(p); id != "" || name != "" {
			t.Fatalf("%s: device %q %q, want none", what, id, name)
		}
	}

	audibles(path)
	wantAudible(path, "Audible's place")
	if rows := feed(); len(rows) != 0 {
		t.Fatalf("Audible's own place fed back: %+v", rows)
	}

	// A write that leaves the place as it was keeps the device: a start date,
	// an edit naming only dates, a position written as it is.
	exec(`UPDATE progress SET started_at = '2026-10-01T00:00:00Z' WHERE user_id = ? AND rel_path = ?`, f.user, path)
	wantAudible(path, "a start date")
	start := f.clock.Add(-48 * time.Hour)
	if _, err := f.c.EditProgress(f.ctx, f.user, ref, ProgressEdit{StartedAt: OptionalTime{Set: true, Value: &start}},
		scopes[0]); err != nil {
		t.Fatal(err)
	}
	wantAudible(path, "an edit naming only a date")
	exec(`UPDATE progress SET position = 1000, version = version + 1 WHERE user_id = ? AND rel_path = ?`, f.user, path)
	wantAudible(path, "the same position written")

	// A writer keeping the device while moving the place: no longer Audible's,
	// and the feed offers it.
	exec(`UPDATE progress SET position = 1500, version = version + 1 WHERE user_id = ? AND rel_path = ?`, f.user, path)
	wantNone(path, "a position moved under Audible's device")
	if rows := feed(); len(rows) != 1 || rows[0].Position != 1500 {
		t.Fatalf("feed after the place moved = %+v", rows)
	}
	audibles(path)
	exec(`UPDATE progress SET finished = 1 WHERE user_id = ? AND rel_path = ?`, f.user, path)
	wantNone(path, "a finish under Audible's device")
	exec(`UPDATE progress SET finished = 0 WHERE user_id = ? AND rel_path = ?`, f.user, path)

	// An insert naming the device (a writer copying it onto a new row).
	exec(`INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, device_name, updated_at)
	      VALUES(?, ?, 'Author/Copy', 10, 36000, 1, 'audible:acct0001', 'Audible (US)', '2026-11-03T08:00:00.000Z')`,
		f.user, f.lib.ID)
	wantNone("Author/Copy", "a new row naming Audible's device")

	// A carried row whose place is the one Audible's sync wrote stays the
	// account's (a folder renamed, a rescan's carry): the feed doesn't offer
	// Audible its own place back, which the Bridge could push over a rewind
	// made in the Audible app since.
	audibles(path)
	if err := f.c.MoveDurableState(f.ctx, f.lib.ID, path, moved); err != nil {
		t.Fatal(err)
	}
	wantAudible(moved, "a moved row")
	if rows := feed(); len(rows) != 0 {
		t.Fatalf("feed after the move = %+v, want Audible's own place left out", rows)
	}
	// So does one merged over an older row an earlier book left there (the
	// carried row wins, its place moving the row the guard watches).
	const again = "Author/Again"
	exec(`INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, device_name, updated_at)
	      VALUES(?, ?, ?, 50, 36000, 1, 'phone', 'Phone', '2020-01-01T00:00:00.000Z')`, f.user, f.lib.ID, again)
	if err := f.c.MoveDurableState(f.ctx, f.lib.ID, moved, again); err != nil {
		t.Fatal(err)
	}
	wantAudible(again, "a moved row merged over an older one")
	if rows := feed(); len(rows) != 0 {
		t.Fatalf("feed after the merge = %+v, want Audible's own place left out", rows)
	}
	// A join's first part, at offset 0 with its place unchanged, is still a
	// place on a longer book, which the joined book's Audible title never gave:
	// no longer the account's, and the feed offers it.
	const folder = "Author"
	if err := f.c.JoinDurableState(f.ctx, f.lib.ID, folder, []JoinPart{{Path: again}}, 0); err != nil {
		t.Fatal(err)
	}
	wantNone(folder, "a join's first part at offset 0")
	if rows := feed(); len(rows) != 1 || rows[0].Path != folder || rows[0].Position != 1500 {
		t.Fatalf("feed after the join = %+v, want the joined book's place offered", rows)
	}
	// So does a join's later part, further on the joined book's timeline.
	exec(`DELETE FROM progress WHERE user_id = ? AND rel_path = ?`, f.user, folder)
	f.savePlace(t, again, 1000, f.clock.Add(-time.Hour))
	audibles(again)
	if err := f.c.JoinDurableState(f.ctx, f.lib.ID, folder, []JoinPart{{Path: again, Offset: 3600, Last: true}},
		0); err != nil {
		t.Fatal(err)
	}
	wantNone(folder, "a row joined at an offset")
}

// A carry names Audible's device again only for a move whose carried row wins
// the merge: never when the row already at the new path is the newer save,
// even with the same place, and never for a join, whatever its part's place
// (the furthest, tied or clamped to the part's length).
func TestAudibleDeviceCarriedOnlyByAMoveItsPlaceWins(t *testing.T) {
	t.Parallel()
	const path = "Author/Book"
	for _, tc := range []struct {
		name, into string
		phone      float64 // the phone's row already at into (0: none)
		phoneAt    string
		join       *JoinPart
		wantDevice string
		wantPos    float64
	}{
		{name: "a move onto a newer save of the same place", into: "Author/Moved", phone: 1000,
			phoneAt: "2099-01-01T00:00:00.000Z", wantDevice: "phone", wantPos: 1000},
		{name: "a move onto an older save", into: "Author/Moved", phone: 500,
			phoneAt: "2020-01-01T00:00:00.000Z", wantDevice: "audible:acct0001", wantPos: 1000},
		{name: "a move onto no row", into: "Author/Moved", wantDevice: "audible:acct0001", wantPos: 1000},
		{name: "a join's first part tied with the folder's place", into: "Author", phone: 1000,
			phoneAt: "2020-01-01T00:00:00.000Z", join: &JoinPart{Path: path}, wantDevice: "phone", wantPos: 1000},
		{name: "a join's first part further than the folder's place", into: "Author", phone: 500,
			phoneAt: "2099-01-01T00:00:00.000Z", join: &JoinPart{Path: path}, wantDevice: "", wantPos: 1000},
		{name: "a join's first part alone", into: "Author", join: &JoinPart{Path: path}, wantDevice: "", wantPos: 1000},
		{name: "a join's first part clamped to its length", into: "Author",
			join: &JoinPart{Path: path, Duration: 900}, wantDevice: "", wantPos: 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := syncedTitleAt(t, path)
			f.savePlace(t, path, 1000, f.clock.Add(-time.Hour))
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.c.db.ExecContext(f.ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE progress SET device_id = 'audible:acct0001', device_name = 'Audible (US)'
			       WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.user, f.lib.ID, path)
			if tc.phone > 0 {
				exec(`INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, device_name, updated_at)
				      VALUES(?, ?, ?, ?, 36000, 1, 'phone', 'Phone', ?)`, f.user, f.lib.ID, tc.into, tc.phone, tc.phoneAt)
			}
			var err error
			if tc.join != nil {
				err = f.c.JoinDurableState(f.ctx, f.lib.ID, tc.into, []JoinPart{*tc.join}, 0)
			} else {
				err = f.c.MoveDurableState(f.ctx, f.lib.ID, path, tc.into)
			}
			if err != nil {
				t.Fatal(err)
			}
			var device string
			var pos float64
			if err := f.c.db.QueryRowContext(f.ctx, `SELECT device_id, position FROM progress
			    WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.user, f.lib.ID, tc.into).Scan(&device, &pos); err != nil {
				t.Fatal(err)
			}
			if device != tc.wantDevice || pos != tc.wantPos {
				t.Fatalf("row at %s = %q at %v, want %q at %v", tc.into, device, pos, tc.wantDevice, tc.wantPos)
			}
		})
	}
}
