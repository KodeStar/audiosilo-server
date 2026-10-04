package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0021Backfill: on a database from before 0021, the players'
// listening spans from before a person's first recorded session become sessions
// (spans 10 minutes apart or less joined, a span that never moved dropped, spans
// after the first session left alone), numbered below the sessions already there
// in the order they happened; what the spans don't cover of a book first saved
// before 0018 becomes one estimated day; a book first saved since, and a demo
// account's, get none.
func TestMigration0021Backfill(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "pre-0021.db")
	raw, err := sql.Open("sqlite", dsnPragmas(dsn))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
		}
	}
	exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries { // sorted by name
		if e.Name() >= "0021" {
			break
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, 't')`, e.Name())
	}
	exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(1, 'ann', '', 't', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at, is_demo) VALUES(2, 'demo_x', '', 't', 't', 1)`)
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	span := func(path string, from, to float64, start, end string) {
		exec(`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at)
			VALUES(1, 1, ?, ?, ?, ?, ?)`, path, from, to, start, end)
	}
	span("A", 0, 1800, "2026-07-01T10:00:00.000Z", "2026-07-01T10:30:00.000Z")
	span("A", 1800, 3300, "2026-07-01T10:35:00.000Z", "2026-07-01T11:00:00.000Z") // 5 min later: same sitting
	span("A", 3300, 4500, "2026-07-02T09:00:00+00:00", "2026-07-02T09:20:00+00:00")
	span("A", 4500, 4500, "2026-07-02T12:00:00.000Z", "2026-07-02T14:00:00.000Z") // never moved
	span("A", 4500, 5100, "2026-09-02T10:00:00.000Z", "2026-09-02T10:10:00.000Z") // after sessions began
	exec(`INSERT INTO listening_sessions(id, user_id, library_id, rel_path, token_id, started_at, last_at, listened)
		VALUES(1, 1, 1, 'A', 5, '2026-09-01T10:00:00.000Z', '2026-09-01T10:10:00.000Z', 600)`)
	progress := func(user int64, path string, pos, dur float64, finished bool, speed float64, updated string, started any) {
		exec(`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed, updated_at, started_at)
			VALUES(?, 1, ?, ?, ?, ?, ?, ?, ?)`, user, path, pos, dur, finished, speed, updated, started)
	}
	progress(1, "A", 9000, 36000, false, 1, "2026-09-02T10:10:00Z", nil)                    // spans 4500 + session 600: 3900 to estimate
	progress(1, "B", 5000, 36000, false, 1, "2026-09-03T10:00:00Z", "2026-09-03T09:00:00Z") // first saved since 0018
	progress(1, "D", 7200, 7200, true, 2, "2026-05-01T20:00:00Z", nil)                      // played to the end at 2x, no spans: 3600
	progress(1, "E", 0, 7200, true, 1, "2026-05-01T20:00:00Z", nil)                         // marked finished, never played: nothing
	progress(2, "C", 9000, 9000, true, 1, "2026-05-01T20:00:00Z", nil)                      // a demo account
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	got := lines(t, db, `SELECT printf('%d %s %s %g %g %g %s %d', id, started_at, last_at, start_pos, end_pos,
		listened, CASE WHEN backfilled THEN 'backfilled' ELSE 'recorded' END, token_id)
		FROM listening_sessions ORDER BY id`)
	want := []string{
		"1 2026-07-01T10:00:00.000Z 2026-07-01T11:00:00.000Z 0 3300 3300 backfilled 0",
		"2 2026-07-02T09:00:00.000Z 2026-07-02T09:20:00.000Z 3300 4500 1200 backfilled 0",
		"3 2026-09-01T10:00:00.000Z 2026-09-01T10:10:00.000Z 0 0 600 recorded 5",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sessions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// New sessions number on from the highest id.
	if _, err := db.writer.ExecContext(ctx, `INSERT INTO listening_sessions(user_id, library_id, rel_path, started_at, last_at)
		VALUES(1, 1, 'A', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	var next int64
	_ = db.writer.QueryRowContext(ctx, `SELECT MAX(id) FROM listening_sessions`).Scan(&next)
	if next != 4 {
		t.Fatalf("next session id = %d, want 4", next)
	}

	got = lines(t, db, `SELECT printf('%s %s %g %d %d', rel_path, day, listened, sessions, estimated)
		FROM listening_daily ORDER BY rel_path`)
	want = []string{
		"A 2026-07-01 3900 0 1", // dated by the book's first (backfilled) session
		"D 2026-05-01 3600 0 1", // dated by its last save
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("estimates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// lines runs q, whose rows are one string each.
func lines(t *testing.T, db *DB, q string) []string {
	t.Helper()
	rows, err := db.writer.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
