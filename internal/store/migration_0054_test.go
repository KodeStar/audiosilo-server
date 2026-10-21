package store

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestMigration0054RekeysKnownShows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0054")
	exec(`INSERT INTO libraries(id,name,root,created_at,kind) VALUES(1,'Pods','/pods','t','podcasts')`)
	exec(`INSERT INTO podcast_shows VALUES(1,'Show','0123456789abcdef','Title','','','','','episodic',0,0,'','','','','',0,NULL,'s','t')`)
	exec(`INSERT INTO podcast_feed_state(library_id,show_path,etag,error_code,secret_missing,failures) VALUES(1,'Show','etag','gone',1,4),(1,'Unbound','lost','timeout',0,2)`)
	closeRaw()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, etag, code string
	var failures, secret int
	if err = db.QueryRowContext(ctx, `SELECT show_id,etag,error_code,failures,secret_missing FROM podcast_feed_state`).Scan(&id, &etag, &code, &failures, &secret); err != nil || id != "0123456789abcdef" || etag != "etag" || code != "gone" || failures != 4 || secret != 1 {
		t.Fatalf("rekey: %s %s %s %d %d %v", id, etag, code, failures, secret, err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM podcast_feed_state`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unbound: %d %v", n, err)
	}
	for _, q := range []string{
		`INSERT INTO podcast_policies VALUES(1,'Show','0123456789abcdef','bad',3,7,'t')`,
		`INSERT INTO podcast_policies VALUES(1,'Show','0123456789abcdef','keep',0,7,'t')`,
		`INSERT INTO podcast_policies VALUES(1,'Show','0123456789abcdef','keep',3,366,'t')`,
	} {
		if _, err = db.ExecContext(ctx, q); err == nil {
			t.Fatalf("accepted %s", q)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO podcast_policies VALUES(1,'Show','0123456789abcdef','keep',3,7,'t'); INSERT INTO podcast_fetch_state(library_id,rel_path,show_id) VALUES(1,'Show/a.mp3','0123456789abcdef'); DELETE FROM libraries WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT count(*) FROM podcast_policies`, `SELECT count(*) FROM podcast_fetch_state`, `SELECT count(*) FROM podcast_feed_state`} {
		if err = db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cascade: %d %v", n, err)
		}
	}
}

func TestMigration0054LateLowerFilename(t *testing.T) {
	t.Parallel()
	// The late migration is a test-only source, not a shipped placeholder for F1.
	// Both orders run through the production executor and preserve real 0054 data.
	for _, lateFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "54_then_53", true: "53_then_54"}[lateFirst], func(t *testing.T) {
			ctx := context.Background()
			dsn, _, closeRaw := openBefore(t, "0054")
			closeRaw()
			source := fstest.MapFS{}
			names, err := migrationNames()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
				if err != nil {
					t.Fatal(err)
				}
				source["migrations/"+name] = &fstest.MapFile{Data: raw}
			}
			late := &fstest.MapFile{Data: []byte(`CREATE TABLE late_merge_test(value TEXT); INSERT INTO late_merge_test VALUES('preserved');`)}
			if lateFirst {
				source["migrations/0053_late_merge_test.sql"] = late
				delete(source, "migrations/0054_podcast_fetch.sql")
			}
			db, err := openUnmigratedForTest(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.migrateFrom(ctx, source); err != nil {
				t.Fatal(err)
			}
			raw, _ := fs.ReadFile(migrationsFS, "migrations/0054_podcast_fetch.sql")
			source["migrations/0054_podcast_fetch.sql"] = &fstest.MapFile{Data: raw}
			source["migrations/0053_late_merge_test.sql"] = late
			if err = db.migrateFrom(ctx, source); err != nil {
				t.Fatal(err)
			}
			if err = db.migrateFrom(ctx, source); err != nil {
				t.Fatal(err)
			}
			var value string
			if err = db.QueryRowContext(ctx, `SELECT value FROM late_merge_test`).Scan(&value); err != nil || value != "preserved" {
				t.Fatalf("late data %q %v", value, err)
			}
			db.Close()
			db, err = Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = db.migrateFrom(ctx, source); err != nil {
				t.Fatal(err)
			}
			var n int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE name IN ('0053_late_merge_test.sql','0054_podcast_fetch.sql')`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("names: %d %v", n, err)
			}
		})
	}
}

// Build a DB using the same SQLite connection setup, before the executor runs.
func openUnmigratedForTest(ctx context.Context, dsn string) (*DB, error) {
	raw, err := sql.Open("sqlite", dsnPragmas(dsn))
	if err != nil {
		return nil, err
	}
	raw.SetMaxOpenConns(1)
	if err = raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return &DB{writer: raw, reader: raw, log: slog.Default(), dsn: dsn}, nil
}

func TestPodcastPolicyBackupRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `INSERT INTO libraries(id,name,root,kind,created_at) VALUES(1,'Pods','/podcasts','podcasts','t'); INSERT INTO podcast_policies VALUES(1,'Show','0123456789abcdef','archive',42,9,'t')`); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "copy.db")
	if err = db.VacuumInto(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(ctx, backup, true); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var mode string
	var keep, days int
	if err = restored.QueryRowContext(ctx, `SELECT mode,keep,days FROM podcast_policies`).Scan(&mode, &keep, &days); err != nil || mode != "archive" || keep != 42 || days != 9 {
		t.Fatalf("backed up policy: %s %d %d %v", mode, keep, days, err)
	}
}
