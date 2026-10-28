package library

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
// without listing ahead, and an unreadable folder reported the same way.
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
		for _, descend := range []func(string, fs.DirEntry) bool{nil, hidden} {
			var got []string
			if err := walkDir(root, descend, record(root, &got, skip)); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s (descend %v): calls\n%s\nwant\n%s", name, descend != nil, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
	}
	if unreadable {
		var calls []string
		_ = walkDir(root, nil, record(root, &calls, skips["none"]))
		if !slices.Contains(calls, "F/locked true d--------- err=true") {
			t.Errorf("the unreadable folder's error call is missing: %q", calls)
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
	err := walkDir(root, nil, record(root, &calls, func(rel string, _ fs.DirEntry) error {
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
	gotErr := walkDir(missing, nil, record(root, &got, nil))
	if !slices.Equal(got, want) || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("missing root: %q %v, want %q %v", got, gotErr, want, wantErr)
	}
}

// Only the folders descend allows are listed ahead.
func TestListAheadOnlyDescended(t *testing.T) {
	t.Parallel()
	root, _ := walkTree(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ahead := listAhead(root, entries, func(p string, _ fs.DirEntry) bool { return filepath.Base(p) != "C" })
	for i, e := range entries {
		listed := ahead[i] != nil
		want := e.IsDir() && e.Name() != "C"
		if listed != want {
			t.Errorf("%s listed ahead = %v, want %v", e.Name(), listed, want)
		}
	}
	if one := listAhead(filepath.Join(root, "B"), mustReadDir(t, filepath.Join(root, "B")), nil); one[0] != nil {
		t.Error("a lone folder was listed ahead")
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
