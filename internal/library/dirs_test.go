package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestListDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, d := range []string{"fiction", "Kids", ".hidden", "audio drama"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(dir, "fiction"), filepath.Join(dir, "linked")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "notes.txt"), filepath.Join(dir, "file-link")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListDirs(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got.Dirs {
		names = append(names, d.Name)
		if d.Path != filepath.Join(dir, d.Name) {
			t.Errorf("%s: path = %q", d.Name, d.Path)
		}
	}
	want := []string{"audio drama", "fiction", "Kids"}
	if runtime.GOOS != "windows" {
		want = []string{"audio drama", "fiction", "Kids", "linked"}
	}
	if len(names) != len(want) {
		t.Fatalf("dirs = %v, want %v (no files, no hidden folders)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("dirs = %v, want %v", names, want)
		}
	}
	if got.Parent != filepath.Dir(dir) || got.Path != dir || got.Truncated {
		t.Errorf("listing = %+v", got)
	}

	capped, err := ListDirs(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Dirs) != 2 || !capped.Truncated {
		t.Errorf("capped listing = %d dirs, truncated=%v", len(capped.Dirs), capped.Truncated)
	}
}

func TestListDirsRejects(t *testing.T) {
	t.Parallel()
	if _, err := ListDirs("relative/path", 0); !errors.Is(err, ErrNotAbsolute) {
		t.Errorf("relative path: err = %v, want ErrNotAbsolute", err)
	}
	if _, err := ListDirs(filepath.Join(t.TempDir(), "gone"), 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing dir: err = %v, want not-exist", err)
	}
}

func TestListDirsRoot(t *testing.T) {
	t.Parallel()
	got, err := ListDirs("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parent != "" || !filepath.IsAbs(got.Path) {
		t.Errorf("root listing = path %q parent %q, want an absolute root with no parent", got.Path, got.Parent)
	}
}

// A listing that doesn't answer (a hung mount) is ErrDirsTimedOut at the
// lister's time-out, and keeps its slot until it returns: with every slot of
// its mount held, another listing there times out too instead of starting a
// reader of its own. Once the hung read returns its slot is free again. A clock
// only bounds the waits: the hung listing never answers on its own.
func TestDirListerBounded(t *testing.T) {
	t.Parallel()
	hang, started := make(chan struct{}), make(chan struct{}, 1)
	var released bool
	defer func() {
		if !released {
			close(hang)
		}
	}()
	l := NewDirLister(func(dir string, _ int) (*DirListing, error) {
		if dir == "/hung" {
			started <- struct{}{}
			<-hang
		}
		return &DirListing{Path: dir}, nil
	}, 1, 50*time.Millisecond)
	l.slots.Mount = func(string) (string, string) { return "/", "/" } // both on one mount
	ctx := context.Background()
	if _, err := l.List(ctx, "/hung", 0); !errors.Is(err, ErrDirsTimedOut) {
		t.Fatalf("hung listing: %v, want ErrDirsTimedOut", err)
	}
	<-started
	if _, err := l.List(ctx, "/fine", 0); !errors.Is(err, ErrDirsTimedOut) {
		t.Fatalf("with the one slot held: %v, want ErrDirsTimedOut", err)
	}
	// A relative path is refused at once, slots held or not.
	if _, err := l.List(ctx, "relative", 0); !errors.Is(err, ErrNotAbsolute) {
		t.Fatalf("a relative path with the one slot held: %v, want ErrNotAbsolute", err)
	}
	close(hang)
	released = true
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := l.List(ctx, "/fine", 0)
		if err == nil && got.Path == "/fine" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the hung read returned: %v, want the listing", err)
		}
	}
}

// A read that gives up when its context ends (a preview's walk) answers with
// that context's error: ErrDirsTimedOut whether the waiter left first or took
// that answer as its time ran out, never the context's own error (a 500 to the
// console, not the 404 of a folder that didn't answer). Any other error a read
// gives stands.
func TestBoundedReadThatEndsWithItsContextTimesOut(t *testing.T) {
	t.Parallel()
	slots := make(chan struct{}, 1)
	for range 20 { // either order of the read's answer and the waiter's wake
		_, err := boundedRead(context.Background(), slots, 5*time.Millisecond, nil, func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})
		if !errors.Is(err, ErrDirsTimedOut) {
			t.Fatalf("a read ended by its context: %v, want ErrDirsTimedOut", err)
		}
	}
	own := errors.New("the read's own")
	if _, err := boundedRead(context.Background(), slots, time.Minute, nil, func(context.Context) (int, error) {
		return 0, own
	}); !errors.Is(err, own) {
		t.Errorf("a read's own error: %v, want it handed back", err)
	}
}

// One hung mount holds only its own slots (server#360's rule for the picker):
// with every slot of /mnt/hung held by listings that don't answer, a listing
// or root check anywhere on it is ErrDirsTimedOut at the time-out, while
// another mount, and the filesystem root above both, list and check at once.
func TestDirListerHungMountLeavesOthers(t *testing.T) {
	t.Parallel()
	hang := make(chan struct{})
	defer close(hang)
	const slots = 2
	started := make(chan struct{}, slots)
	l := NewDirLister(func(dir string, _ int) (*DirListing, error) {
		if strings.HasPrefix(dir, "/mnt/hung") {
			started <- struct{}{}
			<-hang
		}
		return &DirListing{Path: dir}, nil
	}, slots, 50*time.Millisecond)
	l.check = func(root string) (string, error) {
		if strings.HasPrefix(root, "/mnt/hung") {
			<-hang
		}
		return root, nil
	}
	l.slots.Mount = func(p string) (string, string) {
		for _, m := range []string{"/mnt/hung", "/mnt/other"} {
			if p == m || strings.HasPrefix(p, m+"/") {
				return m, m
			}
		}
		return "/", "/"
	}
	ctx := context.Background()
	for range slots { // every slot of the hung mount held
		if _, err := l.List(ctx, "/mnt/hung/Books", 0); !errors.Is(err, ErrDirsTimedOut) {
			t.Fatalf("hung listing: %v, want ErrDirsTimedOut", err)
		}
		<-started
	}
	if _, err := l.List(ctx, "/mnt/hung", 0); !errors.Is(err, ErrDirsTimedOut) {
		t.Errorf("the hung mount with its slots held: %v, want ErrDirsTimedOut", err)
	}
	if _, err := l.CheckRoot(ctx, "/mnt/hung/Other"); !errors.Is(err, ErrDirsTimedOut) {
		t.Errorf("a root check on the hung mount: %v, want ErrDirsTimedOut", err)
	}
	for _, dir := range []string{"/mnt/other/Books", "/mnt", ""} {
		if got, err := l.List(ctx, dir, 0); err != nil || got.Path != dir {
			t.Errorf("listing %q beside the hung mount: %v, %v, want it listed", dir, got, err)
		}
	}
	if got, err := l.CheckRoot(ctx, "/mnt/other/Books"); err != nil || got != "/mnt/other/Books" {
		t.Errorf("a root check on another mount: %q, %v, want it checked", got, err)
	}
	if n := len(started); n != 0 {
		t.Errorf("%d more hung reads started, want none past the slots", n)
	}
}

// The setup preview's root check runs within the same slots and time-out: a
// root that doesn't answer is ErrDirsTimedOut, a relative one refused at once,
// and a readable one comes back cleaned.
func TestDirListerCheckRoot(t *testing.T) {
	t.Parallel()
	hang := make(chan struct{})
	defer close(hang)
	l := NewDirLister(ListDirs, 1, 50*time.Millisecond)
	ctx := context.Background()
	root := t.TempDir()
	if got, err := l.CheckRoot(ctx, root+string(filepath.Separator)); err != nil || got != root {
		t.Fatalf("a readable root: %q, %v, want %q", got, err, root)
	}
	if _, err := l.CheckRoot(ctx, filepath.Join(root, "missing")); !errors.Is(err, ErrRootUnreadable) {
		t.Fatalf("a missing root: %v, want ErrRootUnreadable", err)
	}
	l.check = func(string) (string, error) {
		<-hang
		return "", ErrRootUnreadable
	}
	if _, err := l.CheckRoot(ctx, root); !errors.Is(err, ErrDirsTimedOut) {
		t.Fatalf("a root that doesn't answer: %v, want ErrDirsTimedOut", err)
	}
	if _, err := l.CheckRoot(ctx, "relative"); !errors.Is(err, ErrNotAbsolute) {
		t.Fatalf("a relative root with the one slot held: %v, want ErrNotAbsolute", err)
	}
}

// A listing that panics panics its waiter, not the process (the read runs on a
// goroutine of its own, which nothing would recover), and gives its slot back.
func TestDirListerPanicReachesTheWaiter(t *testing.T) {
	t.Parallel()
	l := NewDirLister(func(dir string, _ int) (*DirListing, error) {
		if dir == "/boom" {
			panic("boom")
		}
		return &DirListing{Path: dir}, nil
	}, 1, 10*time.Second)
	ctx := context.Background()
	func() {
		defer func() {
			if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "boom") {
				t.Errorf("recovered %v, want the listing's panic", p)
			}
		}()
		_, _ = l.List(ctx, "/boom", 0)
	}()
	if got, err := l.List(ctx, "/fine", 0); err != nil || got.Path != "/fine" {
		t.Fatalf("after a panicked listing: %v, want the listing (its slot given back)", err)
	}
}

// A listing that panics after its waiter left with ErrDirsTimedOut has no
// request to reach: it is logged, and its slot still comes back.
func TestDirListerLogsAnAbandonedPanic(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	l := NewDirLister(func(string, int) (*DirListing, error) {
		<-release
		panic("boom")
	}, 1, 20*time.Millisecond)
	var logged bytes.Buffer
	l.log = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := l.List(context.Background(), "/hung", 0); !errors.Is(err, ErrDirsTimedOut) {
		t.Fatalf("a listing that doesn't answer: %v, want ErrDirsTimedOut", err)
	}
	close(release)
	l.slots.For("/hung") <- struct{}{} // waits for the panicked listing to give its slot back
	if got := logged.String(); !strings.Contains(got, "boom") || !strings.Contains(got, "level=ERROR") {
		t.Fatalf("logged %q, want the listing's panic as an error", got)
	}
}
