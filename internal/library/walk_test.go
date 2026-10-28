package library

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// walkTree builds a tree with folders several deep, hidden and lone folders, a
// symlinked folder (never gone into), files beside folders, and an unreadable
// folder where the OS allows one.
func walkTree(t *testing.T) (root string, unreadable bool) {
	t.Helper()
	root = t.TempDir()
	for _, p := range []string{
		"A/Book 1/01.mp3", "A/Book 1/02.mp3", "A/Book 1/cover.jpg", "A/Book 2/CD1/a.mp3", "A/Book 2/CD2/b.mp3",
		"A/notes.txt", "B/Lone/x.m4b", "C/skip/deep/y.mp3", "C/keep/z.mp3", ".hidden/h.mp3", "D/.audiosilo/recaps.json",
		"E/one.mp3", "root.mp3",
	} {
		writeFile(t, root, p, []byte("x"))
	}
	if err := os.Symlink(filepath.Join(root, "A"), filepath.Join(root, "Link")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "F", "locked")
	writeFile(t, root, "F/locked/in.mp3", []byte("x"))
	if err := os.Chmod(locked, 0); err == nil {
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		_, err := os.ReadDir(locked)
		unreadable = err != nil
	}
	return root, unreadable
}

// record is a walk func that writes each call down and skips as told.
func record(root string, calls *[]string, skip func(rel string, d fs.DirEntry) error) fs.WalkDirFunc {
	return func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		kind := "nil"
		if d != nil {
			kind = fmt.Sprint(d.IsDir(), d.Type())
		}
		*calls = append(*calls, fmt.Sprintf("%s %s err=%v", filepath.ToSlash(rel), kind, err != nil))
		if err != nil || d == nil {
			return nil
		}
		return skip(filepath.ToSlash(rel), d)
	}
}

// walkDir makes the same calls as filepath.WalkDir, in the same order, whatever
// fn answers (go on, SkipDir on a folder, SkipDir on a file, SkipAll), with or
// without listing ahead (every folder, the ones descend allows, or none at width
// 1), and an unreadable folder reported the same way.
func TestWalkDirIsWalkDir(t *testing.T) {
	t.Parallel()
	root, unreadable := walkTree(t)
	hidden := func(_ string, d fs.DirEntry) bool { return !strings.HasPrefix(d.Name(), ".") }
	skips := map[string]func(rel string, d fs.DirEntry) error{
		"none": func(string, fs.DirEntry) error { return nil },
		"hidden and C/skip": func(rel string, d fs.DirEntry) error {
			if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || rel == "C/skip") {
				return filepath.SkipDir
			}
			return nil
		},
		"rest of A/Book 1": func(rel string, _ fs.DirEntry) error {
			if rel == "A/Book 1/01.mp3" {
				return filepath.SkipDir // on a file: the rest of its folder
			}
			return nil
		},
		"all after B": func(rel string, _ fs.DirEntry) error {
			if rel == "B" {
				return filepath.SkipAll
			}
			return nil
		},
	}
	for name, skip := range skips {
		var want []string
		if err := filepath.WalkDir(root, record(root, &want, skip)); err != nil {
			t.Fatal(err)
		}
		for _, w := range []struct {
			width   int
			descend func(string, fs.DirEntry) bool
		}{{walkIO, nil}, {walkIO, hidden}, {2, hidden}, {1, nil}} {
			var got []string
			if err := walkDir(t.Context(), root, w.width, w.descend, record(root, &got, skip)); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s (width %d, descend %v): calls\n%s\nwant\n%s", name, w.width, w.descend != nil,
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
	}
	if unreadable {
		for _, width := range []int{walkIO, 1} {
			var calls []string
			_ = walkDir(t.Context(), root, width, nil, record(root, &calls, skips["none"]))
			if !slices.Contains(calls, "F/locked true d--------- err=true") {
				t.Errorf("width %d: the unreadable folder's error call is missing: %q", width, calls)
			}
		}
	}
}

// An error fn returns ends the walk with it; a root that isn't there is fn's
// one call, as WalkDir makes it.
func TestWalkDirErrors(t *testing.T) {
	t.Parallel()
	root, _ := walkTree(t)
	stop := errors.New("stop")
	var calls []string
	err := walkDir(t.Context(), root, walkIO, nil, record(root, &calls, func(rel string, _ fs.DirEntry) error {
		if rel == "A/Book 2" {
			return stop
		}
		return nil
	}))
	if !errors.Is(err, stop) || calls[len(calls)-1] != "A/Book 2 true d--------- err=false" {
		t.Fatalf("err = %v, last call %q", err, calls[len(calls)-1])
	}
	missing := filepath.Join(root, "nope")
	var want, got []string
	wantErr := filepath.WalkDir(missing, record(root, &want, nil))
	gotErr := walkDir(t.Context(), missing, walkIO, nil, record(root, &got, nil))
	if !slices.Equal(got, want) || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("missing root: %q %v, want %q %v", got, gotErr, want, wantErr)
	}
}

// Only the folders descend allows are listed ahead; not a lone folder, and
// none at width 1 (a capped walk's, which may stop at any folder).
func TestListAheadOnlyDescended(t *testing.T) {
	t.Parallel()
	root, _ := walkTree(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ahead := listAhead(t.Context(), root, entries, walkIO, func(p string, _ fs.DirEntry) bool { return filepath.Base(p) != "C" })
	for i, e := range entries {
		listed := ahead[i] != nil
		want := e.IsDir() && e.Name() != "C"
		if listed != want {
			t.Errorf("%s listed ahead = %v, want %v", e.Name(), listed, want)
		}
	}
	if one := listAhead(t.Context(), filepath.Join(root, "B"), mustReadDir(t, filepath.Join(root, "B")), walkIO, nil); one != nil {
		t.Error("a lone folder was listed ahead")
	}
	if none := listAhead(t.Context(), root, entries, 1, nil); none != nil {
		t.Error("folders were listed ahead at width 1")
	}
}

func mustReadDir(t *testing.T, dir string) []fs.DirEntry {
	t.Helper()
	e, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// statAudio reads the size and mtime of each audio file the walk listed and
// counts those gone by the time it looks (a book folder renamed once listed),
// which makes the discovery partial (discoverAuto): a prune then would delete a
// book the next scan finds moved.
func TestStatAudioCountsFilesGone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, p := range []string{"A Book/01.mp3", "A Book/02.mp3", "Z Book/01.mp3"} {
		writeFile(t, root, p, []byte("xyz"))
	}
	lib := catalog.Library{Name: "L", Root: root}
	for _, width := range []int{walkIO, 1} {
		dirs, _, partial, err := audioDirs(t.Context(), lib, root, "", ParseIgnore(nil), slog.New(slog.DiscardHandler), nil, nil, width)
		if err != nil || partial || len(dirs) != 2 {
			t.Fatalf("width %d: walk found %d folders, partial %v, err %v", width, len(dirs), partial, err)
		}
		from, to := filepath.Join(root, "A Book"), filepath.Join(root, "A Book (2019)")
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
		if gone := statAudio(t.Context(), dirs, width); gone != 2 {
			t.Errorf("width %d: %d files gone, want 2", width, gone)
		}
		if info, err := dirs["Z Book"][0].de.Info(); err != nil || info.Size() != 3 {
			t.Errorf("width %d: the file still there = %v, %v", width, info, err)
		}
		if err := os.Rename(to, from); err != nil {
			t.Fatal(err)
		}
	}
}
