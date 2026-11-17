package catalog

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkActivityYear times the admin's Activity page for a year
// (ActivityFor) over 5,000 books and ten listeners who each listened three
// times a day all year and have progress on 300 books (server#225).
//
//	go test ./internal/catalog -run '^$' -bench BenchmarkActivityYear
func BenchmarkActivityYear(b *testing.B) {
	c, ctx := newTestCatalog(b)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 5000 {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: fmt.Sprintf("A%d/B%d.m4b", i%800, i),
			Title: fmt.Sprintf("T %d", i), Author: fmt.Sprintf("A %d", i%800), Format: "m4b", Duration: 36000,
			Size: 1 << 28, AddedAt: now.AddDate(0, 0, -i%700).Format(time.RFC3339)}); err != nil {
			b.Fatal(err)
		}
	}
	exec := func(q string, args ...any) int64 {
		res, err := c.db.ExecContext(ctx, q, args...)
		if err != nil {
			b.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	for u := range 10 {
		uid := exec(`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES(?, 'x', 'user', 't', 't')`,
			fmt.Sprintf("u%d", u))
		for k := range 300 {
			at := now.AddDate(0, 0, -k).Format(time.RFC3339)
			exec(`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed, version, device_id, updated_at, started_at)
				VALUES(?, ?, ?, ?, 36000, ?, 1, 1, 0, ?, ?)`, uid, lib.ID, fmt.Sprintf("A%d/B%d.m4b", (k*7)%800, k*7), float64(k*100), k%3 == 0, at, at)
		}
		for d := range 365 {
			for s := range 3 {
				st := now.AddDate(0, 0, -d).Add(time.Duration(s) * time.Hour)
				exec(`INSERT INTO listening_sessions(user_id, library_id, rel_path, started_at, last_at, listened, duration)
					VALUES(?, ?, ?, ?, ?, 1800, 36000)`, uid, lib.ID, fmt.Sprintf("A%d/B%d.m4b", (d*7)%800, d*7),
					st.Format(time.RFC3339), st.Add(30*time.Minute).Format(time.RFC3339))
			}
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.ActivityFor(ctx, "year", now.AddDate(-1, 0, 0), now, time.UTC); err != nil {
			b.Fatal(err)
		}
	}
}
