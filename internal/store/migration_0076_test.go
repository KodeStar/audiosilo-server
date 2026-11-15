package store

import (
	"context"
	"testing"
)

// TestMigration0076PodcastFeedNotices (server#232): an existing feed state row
// gains feed_failures and failing_announced at 0 (no backfill: a feed already
// failing counts from its next failure), its backoff count kept as it was.
func TestMigration0076PodcastFeedNotices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0076")
	exec(`INSERT INTO libraries(id, name, root, created_at, kind) VALUES(1, 'Pods', '/p', 't', 'podcasts')`)
	exec(`INSERT INTO podcast_shows(library_id, show_path, show_id, title, author, description, link, language, show_type,
		explicit, private, feed_url, feed_host, feed_url_sha256, podcast_guid, cover_path, episode_count, latest_published, feed_sig, indexed_at)
		VALUES(1, 'Show', 'a1b2c3d4e5f60718', 'Show', '', '', '', '', 'episodic', 0, 0, '', '', '', '', '', 0, NULL, 's', 't')`)
	exec(`INSERT INTO podcast_feed_state(library_id, show_path, show_id, error_code, failures) VALUES(1, 'Show', 'a1b2c3d4e5f60718', 'timeout', 4)`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var failures, feedFailures, announced int
	if err := db.writer.QueryRow(`SELECT failures, feed_failures, failing_announced FROM podcast_feed_state`).
		Scan(&failures, &feedFailures, &announced); err != nil {
		t.Fatal(err)
	}
	if failures != 4 || feedFailures != 0 || announced != 0 {
		t.Fatalf("after 0076: failures %d, feed_failures %d, failing_announced %d; want 4, 0, 0", failures, feedFailures, announced)
	}
}
