package catalog

import (
	"reflect"
	"testing"
	"time"
)

// TestAdminRowCoverVersions: the console's rows that name a book without a Book
// carry its cover_version (server#270), the Book's own, so a cover the console
// draws for them is kept across reloads and busted when it changes: listening
// sessions (live and paged), the Overview's listening rows, a person's
// progress, the Activity page's top books and drop-offs, Health's category
// samples (duplicates too) and a match run's book. A path that is no indexed
// book carries none.
func TestAdminRowCoverVersions(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t) // clock: Thu 2026-10-01 09:00 UTC
	if err := f.c.SetCover(f.ctx, f.lib, f.book.Path, pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	_, want := coverState(t, f.c, f.lib, f.book.Path)
	if want == "" {
		t.Fatal("the book has no cover_version")
	}
	// A copy of the book in another folder: a likely duplicate, so Health's
	// duplicates card has a sample.
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib, RelPath: "Copies/Book", IsFolder: true,
		Title: "The Book", Author: "Ann Author", Narrator: "Ned Narrator", Duration: 7200, Size: 1000}); err != nil {
		t.Fatal(err)
	}
	gone := Ref{LibraryID: f.lib, Path: "Gone/Book"}
	bob := f.addUser(t, "bob", "2025-01-01T00:00:00Z", false)
	for _, u := range []int64{f.user, bob} {
		for _, ref := range []Ref{f.book, gone} {
			// Unfinished and idle for weeks: two people left the book in its
			// first chapter, a drop-off.
			if _, err := f.c.SaveProgress(f.ctx, u, Progress{Ref: ref, Position: 60, Duration: 7200,
				UpdatedAt: f.clock.Add(-40 * 24 * time.Hour).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.addSession(t, f.user, 1, f.book, f.clock.Add(-time.Minute), 30*time.Second, 30, "opus", false)
	f.addSession(t, bob, 2, gone, f.clock.Add(-time.Minute), 30*time.Second, 30, "opus", false)

	sessions, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.c.LiveSessions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string][]Session{"sessions": sessions, "live": live} {
		got := map[string]string{}
		for _, s := range list {
			got[s.Path] = s.CoverVersion
		}
		if len(got) != 2 || got[f.book.Path] != want || got[gone.Path] != "" {
			t.Errorf("%s cover versions = %v, want %q on the book and none on %s", name, got, want, gone.Path)
		}
	}

	overview, err := f.c.ListeningOverview(f.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := f.c.ListUserProgress(f.ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, r := range overview {
		rows["overview "+r.Path] = r.CoverVersion
	}
	for _, p := range progress {
		rows["progress "+p.Path] = p.CoverVersion
	}
	if len(rows) != 4 || rows["overview "+f.book.Path] != want || rows["progress "+f.book.Path] != want ||
		rows["overview "+gone.Path] != "" || rows["progress "+gone.Path] != "" {
		t.Errorf("listening rows = %v, want %q on the book's and none on %s", rows, want, gone.Path)
	}

	a, err := f.c.ActivityFor(f.ctx, "7d", f.clock.Add(-7*24*time.Hour), f.clock, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	tops := map[string]string{}
	for _, b := range a.TopBooks {
		tops[b.Path] = b.CoverVersion
	}
	if len(tops) != 2 || tops[f.book.Path] != want || tops[gone.Path] != "" {
		t.Errorf("top books = %v, want %q on the book and none on %s", tops, want, gone.Path)
	}
	if len(a.DropOffs) != 1 || a.DropOffs[0].Path != f.book.Path || a.DropOffs[0].CoverVersion != want {
		t.Errorf("drop-offs = %+v, want the book's with %q", a.DropOffs, want)
	}

	counts, err := f.c.IssueCounts(f.ctx, []string{IssueUnmatched, IssueDuplicate})
	if err != nil {
		t.Fatal(err)
	}
	for _, ic := range counts {
		// Each sample its own book's version (a duplicate group's is its first
		// member, either copy).
		found := false
		for _, s := range ic.Samples {
			_, own := coverState(t, f.c, f.lib, s.Path)
			if s.CoverVersion != own {
				t.Errorf("%s sample %s carries %q, want its book's %q", ic.Kind, s.Path, s.CoverVersion, own)
			}
			found = found || s.Path == f.book.Path
		}
		if ic.Kind == IssueUnmatched && !found {
			t.Errorf("unmatched samples = %+v, want the book among them", ic.Samples)
		}
		if ic.Kind == IssueDuplicate && len(ic.Samples) != 1 {
			t.Errorf("duplicate samples = %+v, want the one group", ic.Samples)
		}
	}

	st, err := f.c.BookMatchState(f.ctx, f.lib, f.book.Path)
	if err != nil || st.CoverVersion != want {
		t.Fatalf("match state = %+v (%v), want cover version %q", st, err, want)
	}
}

// A session on a personal API key says so (the console names the device by its
// key's label, server#224); one on a signed-in device, or on a token gone since,
// doesn't.
func TestSessionAPIKey(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	token := func(kind string) int64 {
		res, err := f.c.db.ExecContext(f.ctx,
			`INSERT INTO tokens(user_id, token_hash, kind, device_name, created_at) VALUES(?, ?, ?, 'cli', ?)`,
			f.user, kind+"-hash", kind, f.clock.Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	key, session := token("api"), token("session")
	f.addSession(t, f.user, key, f.book, f.clock.Add(-time.Minute), 30*time.Second, 30, "", false)
	f.addSession(t, f.user, session, Ref{LibraryID: f.lib, Path: "Other"}, f.clock.Add(-2*time.Minute), 30*time.Second, 30, "", false)
	f.addSession(t, f.user, 9999, Ref{LibraryID: f.lib, Path: "Gone"}, f.clock.Add(-3*time.Minute), 30*time.Second, 30, "", false)
	list, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.c.LiveSessions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, sessions := range map[string][]Session{"sessions": list, "live": live} {
		got := map[int64]bool{}
		for _, s := range sessions {
			got[s.DeviceID] = s.APIKey
		}
		if len(got) != 3 || !got[key] || got[session] || got[9999] {
			t.Errorf("%s api_key by device = %v, want only the key (%d)", name, got, key)
		}
	}
	// The Activity page's apps count the keys that named no app as one row of
	// their own, apart from the other devices that named none.
	a, err := f.c.ActivityFor(f.ctx, "7d", f.clock.Add(-7*24*time.Hour), f.clock, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClientCount{{Devices: 2}, {APIKey: true, Devices: 1}}
	if !reflect.DeepEqual(a.Clients, want) {
		t.Errorf("clients = %+v, want %+v", a.Clients, want)
	}
	// A person's own stats (the player's GET /me/stats) keep them unsplit.
	us, err := f.c.UserStatsFor(f.ctx, "7d", f.clock.Add(-7*24*time.Hour), f.clock, time.UTC, f.user, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []ClientCount{{Devices: 3}}; !reflect.DeepEqual(us.Clients, want) {
		t.Errorf("user stats clients = %+v, want %+v", us.Clients, want)
	}
}
