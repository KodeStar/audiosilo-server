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
