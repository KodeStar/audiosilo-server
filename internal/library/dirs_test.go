package library

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
