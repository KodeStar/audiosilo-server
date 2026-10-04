package library

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func TestStatRoot(t *testing.T) {
	dir := t.TempDir()
	if st := statRoot(dir); !st.readable || !st.empty {
		t.Errorf("empty dir = %+v, want readable and empty", st)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.m4b"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if st := statRoot(dir); !st.readable || st.empty {
		t.Errorf("dir with a file = %+v, want readable, not empty", st)
	}
	if st := statRoot(filepath.Join(dir, "gone")); st.readable {
		t.Errorf("missing dir = %+v, want unreadable", st)
	}
	if st := statRoot(filepath.Join(dir, "a.m4b")); st.readable {
		t.Errorf("a file = %+v, want unreadable (not a directory)", st)
	}
}

// A root on a dead mount can block a stat for minutes. The check gives up after
// rootProbeTimeout, starts no second probe while the first is stuck, and uses the
// late answer once it arrives.
func TestRootProberNeverBlocksTheCaller(t *testing.T) {
	release := make(chan struct{})
	calls := 0
	p := newRootProber()
	p.stat = func(string) rootState {
		calls++
		<-release
		return rootState{readable: true}
	}

	start := time.Now()
	if _, answered := p.check("/mnt/nas"); answered {
		t.Fatal("a blocked probe reported an answer")
	}
	if waited := time.Since(start); waited > rootProbeTimeout+time.Second {
		t.Fatalf("check waited %v, want about %v", waited, rootProbeTimeout)
	}
	p.forget("/mnt/nas") // an in-flight probe is kept, not restarted
	close(release)
	<-p.probes["/mnt/nas"].done // the mount answers
	st, answered := p.check("/mnt/nas")
	if !answered || !st.readable {
		t.Fatalf("after the mount answered: %+v answered=%v", st, answered)
	}
	if calls != 1 {
		t.Fatalf("stat ran %d times, want 1 (no pile-up on a stuck mount)", calls)
	}
}

func TestRootAvailable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	scanner := NewScanner(cat, "", slog.Default())

	if !scanner.RootAvailable(*lib, 0) {
		t.Fatal("a readable root reported unavailable")
	}
	empty := *lib
	empty.Root = t.TempDir()
	if !scanner.RootAvailable(empty, 0) {
		t.Error("an empty root with nothing indexed is just a new library: available")
	}
	if scanner.RootAvailable(empty, 3) {
		t.Error("an empty root with books indexed looks like an unmounted share: unavailable")
	}
	missing := *lib
	missing.Root = filepath.Join(t.TempDir(), "gone")
	if scanner.RootAvailable(missing, 0) {
		t.Error("a missing root reported available")
	}

	// A scan that stops at the guard marks the library unavailable even before
	// the next probe, and a successful rescan clears it.
	if _, err := scanner.Scan(ctx, missing); err == nil {
		t.Fatal("scanning a missing root succeeded")
	}
	if !scanner.Progress(lib.ID).Unavailable {
		t.Fatal("scan progress doesn't record the unavailable root")
	}
	if scanner.RootAvailable(*lib, 0) {
		t.Error("the library reads as available right after its scan hit the guard")
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if scanner.Progress(lib.ID).Unavailable || !scanner.RootAvailable(*lib, 2) {
		t.Error("a successful rescan didn't clear the unavailable state")
	}
}

// A queued scan reads as queued before Enqueue returns, the queue runs it, and the
// run is recorded with what it found.
func TestEnqueueRunsAndRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	scanner := NewScanner(cat, "", slog.Default())

	job := scanner.Enqueue(*lib, TriggerManual, nil)
	if p := scanner.Progress(lib.ID); !p.Queued {
		t.Fatalf("a queued scan doesn't read as queued: %+v", p)
	}
	if again := scanner.Enqueue(*lib, TriggerManual, nil); again.ID != job.ID {
		t.Fatalf("a second ask while queued made job %d, want the queued %d", again.ID, job.ID)
	}
	scanner.Start(ctx)
	waitIdle(t, scanner, lib.ID)
	runs, err := cat.ListScanRuns(ctx, lib.ID, 0, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v (err %v), want one", runs, err)
	}
	if r := runs[0]; r.Status != catalog.RunOK || r.Added == 0 || r.Trigger != TriggerManual || r.FinishedAt == nil {
		t.Fatalf("run = %+v, want a finished ok manual run that added books", r)
	}
	full, err := cat.GetScanRun(ctx, runs[0].ID)
	if err != nil || len(full.Log) == 0 || full.Log[0].Kind != "started" || full.Log[len(full.Log)-1].Kind != "finished" {
		t.Fatalf("log = %+v (err %v), want started ... finished", full.Log, err)
	}
}

// waitIdle waits until a library's scan is neither queued nor running.
func waitIdle(t *testing.T, s *Scanner, libID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, queued := s.Jobs()
		p := s.Progress(libID)
		if running == nil && len(queued) == 0 && !p.Running && !p.Queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue never went idle: %+v", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RootsAvailable probes in parallel: two dead roots cost one timeout, not two.
func TestRootsAvailableInParallel(t *testing.T) {
	s := &Scanner{progress: map[int64]ScanProgress{}, roots: newRootProber()}
	release := make(chan struct{})
	defer close(release)
	s.roots.stat = func(root string) rootState {
		if root == "/ok" {
			return rootState{readable: true}
		}
		<-release
		return rootState{}
	}
	libs := []catalog.Library{{ID: 1, Root: "/ok"}, {ID: 2, Root: "/dead-a"}, {ID: 3, Root: "/dead-b"}}
	start := time.Now()
	got := s.RootsAvailable(libs, map[int64]int{1: 3})
	if took := time.Since(start); took > rootProbeTimeout+time.Second {
		t.Fatalf("took %v, want about one probe timeout", took)
	}
	if !got[1] || got[2] || got[3] {
		t.Fatalf("availability = %v, want only library 1", got)
	}
	// A root still stuck past the timeout answers at once on the next check.
	start = time.Now()
	if _, answered := s.roots.check("/dead-a"); answered || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("a stuck probe made the next check wait (answered=%v, %v)", answered, time.Since(start))
	}
}

// The root probe also reads the space on the root's filesystem, so Health >
// System shows it without another (unbounded) filesystem call.
func TestRootDisk(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())

	d, ok := scanner.RootDisk(catalog.Library{ID: 1, Root: t.TempDir()})
	if !ok || d.Total == 0 || d.Free > d.Total {
		t.Fatalf("RootDisk = %+v, %v", d, ok)
	}
	if _, ok := scanner.RootDisk(catalog.Library{ID: 2, Root: filepath.Join(t.TempDir(), "gone")}); ok {
		t.Fatal("a missing root has no disk to report")
	}
}
