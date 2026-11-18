package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0083KeepsTheAnnouncedCode: a feed already announced failing
// takes its last failed check's code as the code it was announced with, and
// a feed sitting at `gone` is marked announced with it whatever its flag (it
// ended before the upgrade), so a `gone` told before isn't told again
// (server#403); another feed not announced has none. Every show starts with
// no feed.json read stamp.
func TestMigration0083KeepsTheAnnouncedCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0083")
	exec(`INSERT INTO libraries(id, name, root, created_at, kind) VALUES(1, 'Pods', '/p', 't', 'podcasts')`)
	exec(`INSERT INTO podcast_shows(library_id,show_path,show_id,title,author,description,link,language,show_type,explicit,private,feed_url,feed_host,feed_url_sha256,podcast_guid,cover_path,episode_count,latest_published,feed_sig,indexed_at)
		VALUES(1,'Show','0123456789abcdef','Title','','','','','episodic',0,0,'','','','','',0,NULL,'s','t')`)
	exec(`INSERT INTO podcast_feed_state(library_id, show_path, show_id, error_code, failing_announced) VALUES
		(1, 'Gone', '0123456789abcdef', 'gone', 1),
		(1, 'Gone unflagged', '0123456789abcdef', 'gone', 0),
		(1, 'Announced', '0123456789abcdef', 'http_500', 1),
		(1, 'Failing', '0123456789abcdef', 'http_500', 0),
		(1, 'Fine', '0123456789abcdef', '', 0)`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT show_path || ' [' || failing_code || '] ' || failing_announced FROM podcast_feed_state ORDER BY show_path`)
	want := []string{"Announced [http_500] 1", "Failing [] 0", "Fine [] 0", "Gone [gone] 1", "Gone unflagged [gone] 1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("announced codes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := lines(t, db, `SELECT '[' || feed_read_at || ']' FROM podcast_shows`); len(got) != 1 || got[0] != "[]" {
		t.Fatalf("feed_read_at = %v, want empty", got)
	}
}
