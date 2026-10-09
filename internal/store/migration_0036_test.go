package store

import (
	"context"
	"testing"
)

// TestMigration0036CoverSource: the custom covers kept before covers had a source
// are marked community only where the server's records place them - the match
// dialog's audited community save (the newest save of the book, made with the
// cover or since), or a bulk run that applied the book a cover with no audited save
// since - and every other cover (an upload, a cover saved over a community one, an
// upload moved onto a path an older community save names, a run that took no
// cover) stays an upload.
func TestMigration0036CoverSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0036")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	cover := func(path, at string) {
		exec(`INSERT INTO book_covers(library_id, path, mime, updated_at, data) VALUES(1, ?, 'image/png', ?, x'00')`, path, at)
	}
	saved := func(path, at, details string) {
		exec(`INSERT INTO audit_events(at, action, target, details) VALUES(?, 'book.cover_set', ?, ?)`,
			at, "Books: "+path, details)
	}
	// The match dialog's community cover, and one an upload replaced later.
	cover("Dialog", "2026-10-01T10:00:00Z")
	saved("Dialog", "2026-10-01T10:00:00.000Z", `{"bytes":1,"source":"community"}`)
	cover("Replaced", "2026-10-01T11:00:00Z")
	saved("Replaced", "2026-10-01T10:00:00.000Z", `{"bytes":1,"source":"community"}`)
	saved("Replaced", "2026-10-01T11:00:00.000Z", `{"bytes":1}`)
	cover("Uploaded", "2026-10-01T10:00:00Z")
	saved("Uploaded", "2026-10-01T10:00:00.000Z", `{"bytes":1}`)
	// An upload moved onto a path whose last audited save (an earlier book's) was a
	// community one: the record is older than the cover, so it isn't this cover's.
	cover("MovedIn", "2026-10-01T12:00:00.123456789Z")
	saved("MovedIn", "2026-10-01T09:00:00.000Z", `{"bytes":1,"source":"community"}`)
	// The dialog's save in the same millisecond as its cover, the cover's stamp
	// longer as text: still this cover's record.
	cover("SameMilli", "2026-10-01T10:00:00.123456789Z")
	saved("SameMilli", "2026-10-01T10:00:00.123Z", `{"bytes":1,"source":"community"}`)

	// A bulk run that applied covers (fill), and one that couldn't take its cover.
	exec(`INSERT INTO match_runs(id, library_id, mode, status, started_at, scope)
	      VALUES(1, 1, 'match', 'applied', '2026-10-02T09:00:00Z', 'fill')`)
	item := func(path, detail string) {
		exec(`INSERT INTO match_run_items(run_id, library_id, path, outcome, proposal, applied, detail)
		      VALUES(1, 1, ?, 'auto', '{"cover_url":"https://c/x.jpg","values":{}}', 'applied', ?)`, path, detail)
	}
	cover("Bulk", "2026-10-02T09:30:00Z")
	item("Bulk", "")
	cover("BulkFailed", "2026-10-02T09:30:00Z") // an upload; the run's cover failed
	item("BulkFailed", "cover_failed")
	cover("BulkThenUpload", "2026-10-03T09:00:00Z")
	item("BulkThenUpload", "")
	saved("BulkThenUpload", "2026-10-03T09:00:00.000Z", `{"bytes":1}`)
	cover("BeforeRun", "2026-10-01T09:00:00Z") // uploaded before the run began
	item("BeforeRun", "")
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	want := map[string]string{
		"Dialog": "community", "Replaced": "edited", "Uploaded": "edited", "MovedIn": "edited", "SameMilli": "community",
		"Bulk": "community", "BulkFailed": "edited", "BulkThenUpload": "edited", "BeforeRun": "edited",
	}
	for path, source := range want {
		var got string
		if err := db.writer.QueryRow(`SELECT source FROM book_covers WHERE path = ?`, path).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != source {
			t.Errorf("%s: source = %q, want %q", path, got, source)
		}
	}
}
