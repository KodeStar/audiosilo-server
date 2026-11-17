package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
// lister's time-out, and keeps its slot until it returns: with every slot held,
// another listing times out too instead of starting a reader of its own. Once
// the hung read returns its slot is free again. A clock only bounds the waits:
// the hung listing never answers on its own.
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
