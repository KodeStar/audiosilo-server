package library

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// With no ffprobe the in-process probe still reads durations (SCAN-SPIKE
// decision 2, server#269): a book gets its length. A file it is unsure of
// (here, no audio at all) has none, and a rescan by the same server leaves it
// be rather than re-indexing it every time; a restarted server (a new Scanner)
// reads it once more, so a server that ran without ffprobe before gets its
// existing books' lengths from its first scan.
func TestScanWithoutFFprobeReadsDurations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	writeFile(t, root, "Real.m4b", testAudio(t))
	writeFile(t, root, "Unsure.m4b", []byte("no moov here"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "", slog.Default())
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 2 || res.Errors != 0 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	realBook, err := cat.GetBookByPath(ctx, lib.ID, "Real.m4b")
	if err != nil || realBook.Duration <= 0 || realBook.Codec != "aac" {
		t.Fatalf("real book = %+v, %v: want the probe's length", realBook, err)
	}
	unsure, err := cat.GetBookByPath(ctx, lib.ID, "Unsure.m4b")
	if err != nil || unsure.Duration != 0 || unsure.ScanError != "" {
		t.Fatalf("unsure book = %+v, %v: want no length and no problem", unsure, err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v, %v: want nothing re-indexed", res, err)
	}
	// A restart: the book with no length is read once more, then left be.
	again := NewScanner(cat, "", slog.Default())
	if res, err := again.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("first scan after a restart = %+v, %v: want the unsure book read again", res, err)
	}
	if res, err := again.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Fatalf("second scan after a restart = %+v, %v: want nothing re-indexed", res, err)
	}
	// A book an older server indexed with no length (it ran without ffprobe)
	// gets it from the first scan.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 0, codec = '' WHERE rel_path = 'Real.m4b'`); err != nil {
		t.Fatal(err)
	}
	upgraded := NewScanner(cat, "", slog.Default())
	if _, err := upgraded.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if b, _ := cat.GetBookByPath(ctx, lib.ID, "Real.m4b"); b == nil || b.Duration <= 0 || b.Codec != "aac" {
		t.Fatalf("after the upgrade scan = %+v: want its length", b)
	}
}

// A read whose in-process probe ran out of time while another read ran, with no
// ffprobe to fall back to, is read again alone (server#269 item 6), as an
// ffprobe time-out is; one alone isn't.
func TestReadSlotsRetryInProcessTimeoutAlone(t *testing.T) {
	t.Parallel()
	path := testdataRoot(t) + "/Brandon Sanderson/Mistborn/01 - The Final Empire.m4b"
	slots := newReadSlots(2, 5*time.Second)
	var calls atomic.Int32
	var started sync.WaitGroup
	started.Add(2)
	slots.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		if calls.Add(1) <= 2 {
			// The first two reads overlap, and run out of time.
			started.Done()
			started.Wait()
			md, _ := metadata.ExtractTimeout(ctx, p, ffprobe, time.Nanosecond)
			return md
		}
		return extractFile(ctx, p, ffprobe, timeout)
	}
	var wg sync.WaitGroup
	mds := make([]*metadata.Metadata, 2)
	for i := range mds {
		wg.Go(func() { mds[i] = slots.extract(context.Background(), path, "") })
	}
	wg.Wait()
	for i, md := range mds {
		if md == nil || md.ProbeTimedOut() || md.Duration <= 0 {
			t.Errorf("read %d = %+v: want it read again alone, with its length", i, md)
		}
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("%d reads, want 2 and a retry of each", n)
	}

	// Alone, a time-out is the file's own: no retry.
	lone := newReadSlots(2, 5*time.Second)
	var loneCalls atomic.Int32
	lone.readFile = func(ctx context.Context, p, ffprobe string, _ time.Duration) *metadata.Metadata {
		loneCalls.Add(1)
		md, _ := metadata.ExtractTimeout(ctx, p, ffprobe, time.Nanosecond)
		return md
	}
	if md := lone.extract(context.Background(), path, ""); !md.ProbeTimedOut() || loneCalls.Load() != 1 {
		t.Errorf("a lone time-out: %+v after %d reads, want it kept after one", md, loneCalls.Load())
	}
}
