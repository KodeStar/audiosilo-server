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
	c, ctx := newTestCatalog(t)
	c.now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	user := seedUser(t, c, ctx)
	ref := Ref{LibraryID: lib.ID, Path: "Author/Book"}
	p, err := c.SaveProgress(ctx, user, Progress{Ref: ref, Position: 60, Duration: 600,
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
