package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0090CarriesChecksOverTheMatch: a community chapter check made
// before the recorded match joined its basis (server#375) takes the match
// into it when the match names the recording it came from, or names none and
// is of that recording's work, so the upgrade leaves it in use. A check of
// another recording or work, one that found nothing and an unmatched book's
// keep their basis: the first three are stale and checked again by the match.
func TestMigration0090CarriesChecksOverTheMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0090")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO community_chapters(library_id, path, basis, status, work_id, recording_id, checked_at) VALUES
		(1, 'Same', 'b1', 'fill', 'w', 'r', 't'),
		(1, 'Other recording', 'b2', 'fill', 'w', 'r', 't'),
		(1, 'No recording', 'b3', 'unavailable', 'w', 'r', 't'),
		(1, 'Other work', 'b4', 'fill', 'w', 'r', 't'),
		(1, 'Nothing found', 'b5', 'no_match', '', '', 't'),
		(1, 'Unmatched', 'b6', 'fill', 'w', 'r', 't')`)
	exec(`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, set_at) VALUES
		(1, 'Same', 'w', 'r', 'auto', 't'),
		(1, 'Other recording', 'w', 'r2', 'accepted', 't'),
		(1, 'No recording', 'w', '', 'accepted', 't'),
		(1, 'Other work', 'w2', 'r', 'accepted', 't'),
		(1, 'Nothing found', 'w', 'r', 'auto', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT path || ' ' || replace(basis, char(30), '^') FROM community_chapters ORDER BY basis`)
	want := []string{
		"Same b1^w/r",
		"Other recording b2",
		"No recording b3^w/",
		"Other work b4",
		"Nothing found b5",
		"Unmatched b6",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bases:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
