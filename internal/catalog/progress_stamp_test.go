package catalog

import (
	"testing"
	"time"
)

// server#224: a PUT's updated_at is stored as UTC milliseconds whatever form
// the client sent (an offset, microseconds), as every other progress write
// stamps it, so the Activity funnel's text compare counts it in its period: a
// save half a second into a day, sent as +02:00, is that day's, not the one
// before.
func TestSaveProgressStoresUpdatedAtAsUTC(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.clock = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	c, ctx, user := f.c, f.ctx, f.user
	p, err := c.SaveProgress(ctx, user, Progress{Ref: f.book, Position: 60, Duration: 600,
		UpdatedAt: "2026-10-25T02:00:00.500123+02:00"})
	if err != nil {
		t.Fatal(err)
	}
	const want = "2026-10-25T00:00:00.500Z"
	if p.UpdatedAt != want {
		t.Fatalf("echoed updated_at %q, want %q", p.UpdatedAt, want)
	}
	var stored string
	if err := c.db.QueryRowContext(ctx, `SELECT updated_at FROM progress WHERE user_id = ?`, user).Scan(&stored); err != nil || stored != want {
		t.Fatalf("stored updated_at %q, %v; want %q", stored, err, want)
	}
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		from time.Time
		want int
	}{{day, 1}, {day.Add(-24 * time.Hour), 0}} {
		var a Activity
		if err := c.activityFunnel(ctx, &a, tc.from, tc.from.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if a.Funnel.Started != tc.want {
			t.Fatalf("funnel from %s started %d, want %d", tc.from, a.Funnel.Started, tc.want)
		}
	}
}

// server#224: a book counts as a drop-off once two people left it unfinished
// for 30 days; its cutoff is compared with updated_at in the stored form
// (fixed-width UTC milliseconds), so a row saved half a second after the cutoff
// is still fresh, and one half a second before it is stalled.
func TestActivityDropOffCutoffInStoredForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		saved time.Duration // against the cutoff
		want  int
	}{{"after the cutoff", 500 * time.Millisecond, 0}, {"before the cutoff", -500 * time.Millisecond, 1}} {
		f := newSessionFixture(t)
		stalled := Ref{LibraryID: f.lib, Path: "Stalled"}
		if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib, RelPath: "Stalled", IsFolder: true, Title: "Stalled",
			Duration: 600, Size: 500, Format: "mp3", Codec: "mp3", AddedAt: "2026-01-15T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		at := FormatStamp(f.clock.Add(-dropOffIdle + tc.saved))
		for _, u := range []int64{f.user, f.addUser(t, "bob", "2025-01-01T00:00:00Z", false)} {
			if _, err := f.c.SaveProgress(f.ctx, u, Progress{Ref: stalled, Position: 400, Duration: 600, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		var a Activity
		if err := f.c.activityDropOffs(f.ctx, &a, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if len(a.DropOffs) != tc.want {
			t.Fatalf("%s: drop-offs %+v, want %d", tc.name, a.DropOffs, tc.want)
		}
	}
}
