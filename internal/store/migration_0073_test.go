package store

import (
	"context"
	"database/sql"
	"testing"
)

// TestMigration0073BackfillsEpisodeStamps (server#182): an episode row indexed
// before 0073 takes its books row's indexed_at, so a rollback's rewrite of that
// books row is told from the first scan after it, even for a show whose feed
// never changes again.
func TestMigration0073BackfillsEpisodeStamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0073")
	exec(`INSERT INTO libraries(id,name,root,created_at,kind) VALUES(1,'Pods','/pods','t','podcasts')`)
	exec(`INSERT INTO books(id,library_id,rel_path,title,indexed_at,added_at) VALUES(7,1,'Show/a.mp3','A','2026-11-01T00:00:00Z','t')`)
	exec(`INSERT INTO podcast_episodes(book_id,library_id,show_path,episode_id,guid,episode_type,explicit,feed_duration,present,generation,removed_at,duration_source)
	      VALUES(7,1,'Show','ep-a','guid-a','full',0,0,1,1,'','feed')`)
	closeRaw()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stamp sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT indexed_at FROM podcast_episodes WHERE book_id=7`).Scan(&stamp); err != nil ||
		stamp.String != "2026-11-01T00:00:00Z" {
		t.Fatalf("episode stamp = %+v, %v; want the books row's", stamp, err)
	}
}
