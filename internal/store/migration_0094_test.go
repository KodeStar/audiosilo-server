package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func TestMigration0094CoverColorArt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, exec, closeRaw := store.OpenBefore(t, "0094")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	const art = "s123-456"
	tag := catalog.CoverVersion(art)
	old := catalog.CoverVersion("s123-111")
	for _, b := range []struct{ path, art, color string }{
		{"palette", art, tag + " #123456"},
		{"no colour", art, tag},
		{"stale palette", art, old + " #987654"},
		{"stale no colour", art, old},
		{"unread", art, ""},
		{"no identity", "", ""},
	} {
		exec(`INSERT INTO books(library_id, rel_path, title, cover_art, cover_color, indexed_at)
            VALUES(1, ?, 'T', ?, ?, 't')`, b.path, b.art, b.color)
	}
	closeRaw()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := catalog.New(db, time.Now)
	marker := func() (n int) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM server_state WHERE key = 'backfill_cover_color_art_0094'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if marker() != 1 {
		t.Fatal("0094 left no backfill marker")
	}
	// Twice: the marker goes with the verified identities, so the second is a no-op.
	for range 2 {
		if err := c.BackfillCoverColorArt(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if marker() != 0 {
		t.Fatal("backfill marker left behind")
	}
	checkDue := func(want ...string) {
		t.Helper()
		rows, next, err := c.CoverColorsDue(ctx, 0, 10)
		if err != nil || next != 0 {
			t.Fatalf("due page: next=%d err=%v", next, err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.Path)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("due = %q, want %q", got, want)
		}
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE cover_color_art IS cover_art`).Scan(&current); err != nil || current != 2 {
		t.Fatalf("backfilled current readings = %d, want 2: %v", current, err)
	}
	checkDue("stale palette", "stale no colour", "unread", "no identity")
	// A rolled-back server refreshes a stale palette with its original two-column write.
	if _, err := db.ExecContext(ctx, `UPDATE books SET cover_art = ?, cover_color = ?
        WHERE rel_path = 'stale palette' AND cover_art = ?`, "s456-789", catalog.CoverVersion("s456-789"), art); err != nil {
		t.Fatal(err)
	}
	checkDue("stale no colour", "unread", "no identity")
	if _, err := db.ExecContext(ctx, `UPDATE books SET cover_art = ? WHERE rel_path = 'stale palette'`, art); err != nil {
		t.Fatal(err)
	}
	checkDue("stale palette", "stale no colour", "unread", "no identity")
	// The legacy colour was stale at upgrade, but its original file returned
	// before the pass ran. The palette shown on the book is already current.
	if _, err := db.ExecContext(ctx, `UPDATE books SET cover_art = 's123-111' WHERE rel_path = 'stale no colour'`); err != nil {
		t.Fatal(err)
	}
	checkDue("stale palette", "unread", "no identity")
	// A cleared colour is no reading: the book is due again.
	if _, err := db.ExecContext(ctx, `UPDATE books SET cover_color = '' WHERE rel_path = 'palette'`); err != nil {
		t.Fatal(err)
	}
	checkDue("palette", "stale palette", "unread", "no identity")
}
