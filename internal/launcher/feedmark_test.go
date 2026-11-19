package launcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/jsonfile"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// The progress feed's end is written down in the data folder, at start, while
// it moves and at the stop, and a start that finds the database behind it (a
// backup put in place by hand: no restore recorded, the feed's id kept) renews
// the feed, so a cursor the counter has passed again is refused rather than
// paged on from (bridge#50).
func TestProgressFeedMark(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	dir := t.TempDir()
	path := filepath.Join(dir, progressFeedMarkFile)
	feed := func() string {
		t.Helper()
		v, err := store.GetState[string](ctx, db, "progress_feed")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	counter := func(n int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE progress_change_counter SET seq = ? WHERE id = 1`, n); err != nil {
			t.Fatal(err)
		}
	}
	marked := func(name string, want int64) {
		t.Helper()
		got, err := jsonfile.Read[progressFeedMark](path)
		if err != nil || got == nil || got.End != want {
			t.Fatalf("%s: mark = %+v, %v; want %d", name, got, err, want)
		}
	}
	open := func(renewed bool) *feedMark {
		t.Helper()
		m := openProgressFeedMark(ctx, cat, dir, renewed, discardLog())
		if m == nil {
			t.Fatal("no mark kept")
		}
		return m
	}
	if err := store.PutState(ctx, db, "progress_feed", "the-first", "2026-11-19T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// A first start: nothing to compare with, the end written down.
	counter(5)
	m := open(false)
	marked("a first start", 5)
	// While the server runs the mark follows the feed, and the stop writes it
	// a last time, on a context that has ended by then.
	counter(9)
	m.save(ctx)
	marked("a save", 9)
	running, stopRun := context.WithCancel(ctx)
	stop := m.keep(running)
	counter(12)
	stopRun()
	stop()
	marked("the stop", 12)
	if feed() != "the-first" {
		t.Fatal("a database at its mark had its progress feed renewed")
	}

	// The next start finds a database behind the mark: renewed, once.
	counter(7)
	open(false)
	renewed := feed()
	if renewed == "the-first" {
		t.Fatal("a database behind its mark kept its progress feed id")
	}
	marked("a database behind its mark", 7)
	open(false)
	if feed() != renewed {
		t.Fatal("the start after renewed the progress feed again")
	}

	// A start that renewed for a restore already has nothing more to renew for.
	if err := jsonfile.Write(path, progressFeedMark{End: 30}); err != nil {
		t.Fatal(err)
	}
	open(true)
	if feed() != renewed {
		t.Fatal("a restore's renewal was made twice")
	}
	marked("after a restore's renewal", 7)

	// A renewal that failed leaves the mark for the next start.
	if err := jsonfile.Write(path, progressFeedMark{End: 30}); err != nil {
		t.Fatal(err)
	}
	ended, end := context.WithCancel(ctx)
	end()
	if m := openProgressFeedMark(ended, cat, dir, false, discardLog()); m != nil {
		t.Fatal("a mark kept after a check that failed")
	}
	marked("a check that failed", 30)
	open(false)
	if feed() == renewed {
		t.Fatal("the start after a failed check never renewed the progress feed")
	}
	renewed = feed()

	// A mark that can't be read is written again, and renews nothing.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	open(false)
	marked("an unreadable mark", 7)
	if feed() != renewed {
		t.Fatal("an unreadable mark renewed the progress feed")
	}
}

// A real start renews the Bridge's progress feed for a database behind its
// mark, and for a restore whose own start ended before the renewal (its
// outcome on record, the marker gone), and leaves the mark at the feed's end.
func TestRunRenewsTheProgressFeed(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	mark := filepath.Join(data, progressFeedMarkFile)
	// state reads the feed's id after putting the backup's in its place.
	state := func(put bool) string {
		t.Helper()
		db, err := store.Open(ctx, filepath.Join(data, "audiosilo.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if put {
			if err := store.PutState(ctx, db, "progress_feed", "the-backups", "2026-11-19T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
		}
		id, err := store.GetState[string](ctx, db, "progress_feed")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	run := func() {
		t.Helper()
		writeOffConfig(t, data, freeAddr(t), "")
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- Run(runCtx, Options{DataDir: data, Log: discardLog(), banners: &bytes.Buffer{}}) }()
		waitFor(t, "the healthcheck", func() bool {
			var out bytes.Buffer
			return Healthcheck(runCtx, data, &out) == nil && strings.HasPrefix(out.String(), "ok: ")
		})
		stop()
		waitRun(t, done)
	}

	// The database is behind the mark an earlier run left.
	state(true)
	if err := jsonfile.Write(mark, progressFeedMark{End: 30}); err != nil {
		t.Fatal(err)
	}
	run()
	if state(false) == "the-backups" {
		t.Fatal("a start with the database behind its mark kept the progress feed's id")
	}
	if got, err := jsonfile.Read[progressFeedMark](mark); err != nil || got == nil || got.End != 0 {
		t.Fatalf("mark after the start = %+v, %v; want the feed's end, 0", got, err)
	}

	// A restore applied by a start that ended before it renewed.
	state(true)
	if err := jsonfile.Write(filepath.Join(data, "restore-result.json"),
		backup.RestoreResult{Name: "audiosilo-a.db", AppliedAt: time.Now().UTC(), OK: true}); err != nil {
		t.Fatal(err)
	}
	run()
	if state(false) == "the-backups" {
		t.Fatal("the start after a restore's own ended early kept the progress feed's id")
	}
}
