package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func (f *sessionFixture) addUser(t *testing.T, name, created string, demo bool) int64 {
	t.Helper()
	res, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at, is_demo) VALUES(?, 'x', 'user', ?, ?, ?)`,
		name, created, created, demo)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *sessionFixture) addSession(t *testing.T, user, token int64, ref Ref, start time.Time, length time.Duration, listened float64, codec string, transcoded bool) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO listening_sessions(user_id, library_id, rel_path, token_id, started_at, last_at, listened, codec, transcoded)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		user, ref.LibraryID, ref.Path, token, formatSessionTime(start), formatSessionTime(start.Add(length)),
		listened, codec, transcoded); err != nil {
		t.Fatal(err)
	}
}

func (f *sessionFixture) addClientSession(t *testing.T, user, token int64, app, version, platform string) {
	t.Helper()
	start := f.clock.Add(-time.Hour)
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO listening_sessions(user_id, library_id, rel_path, token_id, started_at, last_at, listened,
		     client_app, client_version, client_platform) VALUES(?,?,?,?,?,?,60,?,?,?)`,
		user, f.lib, f.book.Path, token, formatSessionTime(start), formatSessionTime(start.Add(time.Minute)),
		app, version, platform); err != nil {
		t.Fatal(err)
	}
}

func (f *sessionFixture) addToken(t *testing.T, user int64, app, version, platform string, lastSeen time.Time) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO tokens(user_id, token_hash, kind, created_at, last_seen, client_app, client_version, client_platform)
		 VALUES(?, ?, 'session', ?, ?, ?, ?, ?)`,
		user, app+version+lastSeen.String(), lastSeen.Format(time.RFC3339), lastSeen.Format(time.RFC3339),
		app, version, platform); err != nil {
		t.Fatal(err)
	}
}

func TestActivityFor(t *testing.T) {
	f := newSessionFixture(t) // clock: Thu 2026-10-01 09:00 UTC
	ann := f.user
	bob := f.addUser(t, "bob", "2025-01-01T00:00:00Z", false)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 1, h, m, 0, 0, time.UTC) }

	f.addSession(t, ann, 1, f.book, at(8, 0), 30*time.Minute, 1800, "opus", false)
	f.addSession(t, bob, 2, f.book, at(8, 15), 30*time.Minute, 1200, "opus", true)
	f.addSession(t, bob, 2, f.book, at(8, 0).AddDate(0, 0, -10), 10*time.Minute, 600, "opus", false) // previous week
	f.clock = at(8, 30)
	if _, err := f.c.SaveProgress(f.ctx, ann, Progress{Ref: f.book, Position: 7200, Duration: 7200, Finished: true,
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.SaveProgress(f.ctx, bob, Progress{Ref: f.book, Position: 3700, Duration: 7200,
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.clock = at(9, 0)

	a, err := f.c.ActivityFor(f.ctx, "7d", f.clock.AddDate(0, 0, -7), f.clock, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if a.Totals != (ActivityTotals{Listened: 3000, Sessions: 2, Listeners: 2, Books: 1, Finished: 1}) {
		t.Fatalf("totals = %+v", a.Totals)
	}
	if a.Previous.Listened != 600 || a.Previous.Sessions != 1 || a.Previous.Listeners != 1 {
		t.Fatalf("previous = %+v", a.Previous)
	}
	if len(a.Days) != 8 || a.Days[7].Date != "2026-10-01" || a.Days[7].Listened != 3000 || len(a.Days[7].ByUser) != 2 || a.Days[0].Listened != 0 {
		t.Fatalf("days = %+v", a.Days)
	}
	if a.HourWeekday[3][8] != 3000 { // Thursday, 08:00
		t.Fatalf("hour x weekday at Thu 08h = %v", a.HourWeekday[3][8])
	}
	if len(a.TopBooks) != 1 || a.TopBooks[0].Title != "The Book" || a.TopBooks[0].Listeners != 2 {
		t.Fatalf("top books = %+v", a.TopBooks)
	}
	if len(a.TopAuthors) != 1 || a.TopAuthors[0].Name != "Ann Author" || len(a.TopNarrators) != 1 {
		t.Fatalf("top people = %+v / %+v", a.TopAuthors, a.TopNarrators)
	}
	if len(a.TopUsers) != 2 || a.TopUsers[0].UserID != ann || a.TopUsers[0].Finished != 1 || a.TopUsers[1].Sessions != 1 {
		t.Fatalf("top users = %+v", a.TopUsers)
	}
	if len(a.Playback) != 2 || a.Playback[0] != (PlaybackShare{Codec: "opus", Listened: 1800, Sessions: 1}) ||
		a.Playback[1] != (PlaybackShare{Transcoded: true, Codec: "opus", Listened: 1200, Sessions: 1}) {
		t.Fatalf("playback = %+v", a.Playback)
	}
	if a.PeakConcurrent.Streams != 2 || a.PeakConcurrent.At == nil || *a.PeakConcurrent.At != "2026-10-01T08:15:00Z" {
		t.Fatalf("peak = %+v", a.PeakConcurrent)
	}
	if a.Funnel != (Funnel{Started: 2, Reached25: 2, Reached50: 2, Reached75: 1, Finished: 1}) {
		t.Fatalf("funnel = %+v", a.Funnel)
	}
	if a.Timezone != "UTC" || a.UTCOffset != 0 || a.Range != "7d" {
		t.Fatalf("labels = %q %d %q", a.Timezone, a.UTCOffset, a.Range)
	}
}

func TestActivityYearReadsRolledUpDays(t *testing.T) {
	f := newSessionFixture(t)
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions) VALUES('2026-01-05', ?, ?, ?, 900, 2)`,
		f.user, f.lib, f.book.Path); err != nil {
		t.Fatal(err)
	}
	label, from, to, err := ParseActivityRange("2026", f.clock, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.c.ActivityFor(f.ctx, label, from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if a.Totals.Listened != 900 || a.Totals.Sessions != 2 || len(a.TopBooks) != 1 || a.Days[4].Listened != 900 {
		t.Fatalf("a rolled-up day counts toward the year: totals %+v, day %+v", a.Totals, a.Days[4])
	}
	var hw float64
	for _, row := range a.HourWeekday {
		for _, v := range row {
			hw += v
		}
	}
	if hw != 0 || len(a.Playback) != 0 {
		t.Fatal("a rolled-up day has no hour or playback mode")
	}
}

func TestActivityCollectionAndPeople(t *testing.T) {
	f := newSessionFixture(t) // clock 2026-10-01 09:00 UTC; "Author/Book" added_at = now-ish
	yes := true
	stalled := Ref{LibraryID: f.lib, Path: "Stalled"}
	if _, err := f.c.UpsertBook(f.ctx, &Book{
		LibraryID: f.lib, RelPath: "Stalled", IsFolder: true, Title: "Stalled", Duration: 600, Size: 500,
		Format: "mp3", Codec: "mp3", ASIN: "B000000001", HasCover: &yes, AddedAt: "2026-01-15T00:00:00Z",
		ScanError: "empty_file",
		Chapters:  []metadata.Chapter{{Index: 0, Title: "One"}, {Index: 1, Title: "Two", BookOffset: 300}},
	}); err != nil {
		t.Fatal(err)
	}
	bob := f.addUser(t, "bob", "2025-01-01T00:00:00Z", false)
	old := f.clock.AddDate(0, 0, -40).Format(time.RFC3339)
	for _, u := range []int64{f.user, bob} {
		if _, err := f.c.SaveProgress(f.ctx, u, Progress{Ref: stalled, Position: 400, Duration: 600, UpdatedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	f.addToken(t, f.user, "AudioSilo", "1.6.0", "ios", f.clock.Add(-time.Hour))
	f.addToken(t, bob, "AudioSilo", "1.4.2", "ios", f.clock.Add(-2*time.Hour))
	// Apps in use come from the sessions, with the app each recorded at the time.
	f.addClientSession(t, f.user, 101, "AudioSilo", "1.4.2", "ios")
	f.addClientSession(t, bob, 102, "AudioSilo", "1.4.2", "ios")
	f.addClientSession(t, bob, 102, "AudioSilo", "1.4.2", "ios")
	f.addClientSession(t, bob, 103, "", "", "")
	gone := f.addUser(t, "gone", "2025-01-01T00:00:00Z", false)
	f.addToken(t, gone, "AudioSilo", "1.2.0", "android", f.clock.AddDate(0, 0, -90))
	f.addUser(t, "never", "2025-01-01T00:00:00Z", false)
	f.addUser(t, "new", f.clock.Format(time.RFC3339), false)
	f.addUser(t, "demo_x", "2025-01-01T00:00:00Z", true)

	a, err := f.c.ActivityFor(f.ctx, "30d", f.clock.AddDate(0, 0, -30), f.clock, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.DropOffs) != 1 || a.DropOffs[0].Chapter != "Two" || a.DropOffs[0].ChapterIndex != 1 ||
		a.DropOffs[0].Listeners != 2 || !a.DropOffs[0].ScanError {
		t.Fatalf("drop-offs = %+v", a.DropOffs)
	}
	if len(a.Clients) != 2 || a.Clients[0] != (ClientCount{App: "AudioSilo", Version: "1.4.2", Platform: "ios", Devices: 2}) ||
		a.Clients[1].App != "" {
		t.Fatalf("clients = %+v", a.Clients)
	}
	if a.Storage.Bytes != 1500 || len(a.Storage.ByLibrary) != 1 || a.Storage.ByLibrary[0].Books != 2 || len(a.Storage.ByCodec) != 2 {
		t.Fatalf("storage = %+v", a.Storage)
	}
	if a.Coverage != (Coverage{Books: 2, Identified: 1, WithChapters: 2, WithCover: 1}) {
		t.Fatalf("coverage = %+v", a.Coverage)
	}
	var names []string
	for _, u := range a.InactiveUsers {
		names = append(names, u.Username)
	}
	if len(names) != 2 || names[0] != "never" || names[1] != "gone" {
		t.Fatalf("inactive = %v, want [never gone]", names)
	}
	if len(a.Growth) != 31 || a.Growth[0].Books != 1 || a.Growth[len(a.Growth)-1].Books != 2 {
		t.Fatalf("growth = %d points, first %+v, last %+v", len(a.Growth), a.Growth[0], a.Growth[len(a.Growth)-1])
	}
}

func TestParseActivityRange(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 59, 59, 500, time.UTC) // rounds up to 09:00:00
	end := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in, label string
		days      int
	}{{"", "30d", 30}, {"7d", "7d", 7}, {"90d", "90d", 90}, {"1y", "1y", 365}} {
		label, from, to, err := ParseActivityRange(tc.in, now, time.UTC)
		if err != nil || label != tc.label || !to.Equal(end) || !from.Equal(end.AddDate(0, 0, -tc.days)) {
			t.Errorf("%q: %q %v..%v %v", tc.in, label, from, to, err)
		}
	}
	_, from, to, err := ParseActivityRange("2025", now, time.UTC)
	if err != nil || !from.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("2025: %v..%v %v", from, to, err)
	}
	if _, _, to, _ := ParseActivityRange("2026", now, time.UTC); !to.Equal(end) {
		t.Fatalf("the current year ends now, got %v", to)
	}
	for _, bad := range []string{"2d", "2027", "1999", "abcd", "30"} {
		if _, _, _, err := ParseActivityRange(bad, now, time.UTC); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("%q: err = %v, want ErrInvalidRange", bad, err)
		}
	}
}

// TestActivityBoundaryHourCountedOnce: with a period that starts mid-hour, the
// hour holding its start belongs to the current period only; the previous
// period's totals must not count it too.
func TestActivityBoundaryHourCountedOnce(t *testing.T) {
	f := newSessionFixture(t) // clock: 2026-10-01 09:00 UTC
	from := time.Date(2026, 9, 24, 9, 30, 0, 0, time.UTC)
	f.addSession(t, f.user, 1, f.book, time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC), 50*time.Minute, 600, "", false)
	a, err := f.c.ActivityFor(f.ctx, "7d", from, from.AddDate(0, 0, 7), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Totals.Listened + a.Previous.Listened; got != 600 {
		t.Fatalf("600 s listened in the boundary hour counted as %v (current %v, previous %v)",
			got, a.Totals.Listened, a.Previous.Listened)
	}
}
