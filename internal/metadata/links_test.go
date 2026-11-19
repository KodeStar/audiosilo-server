package metadata

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// junctionInfo is an Lstat's answer for a Windows junction since Go 1.23:
// irregular, neither a link nor a folder (LinkKind's maybe).
type junctionInfo struct{ fs.FileInfo }

func (junctionInfo) Mode() fs.FileMode { return fs.ModeIrregular | 0o777 }
func (junctionInfo) IsDir() bool       { return false }

// junctionLooks are walkLinks' looks with the links at the paths in junctions
// read as Windows reads a junction (junctionInfo), and the files in irregular
// as another reparse point (irregular too, and no link: readlink's EINVAL).
func junctionLooks(junctions, irregular map[string]bool) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		info, err := os.Lstat(p)
		if err == nil && (junctions[p] || irregular[p]) {
			return junctionInfo{info}, nil
		}
		return info, err
	}
}

// TestWalkLinksFollowsJunctions (security, Windows' resolve): a junction
// inside a library is followed as a link (its absolute target walked in its
// place), so a file below it resolves to its real path under the root and
// opens there; one whose target is out of the library resolves out of it,
// which every caller refuses (relBelow). Another reparse point that reads as
// no link is a plain name, a file along a path is ENOTDIR, and a loop is an
// error. Run here with links read as junctions (junctionLooks), as no
// junction can be made off Windows.
func TestWalkLinksFollowsJunctions(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	inLib := filepath.Join(root, "Shelf")
	out := filepath.Join(root, "Away")
	loop := filepath.Join(root, "Loop")
	for link, target := range map[string]string{
		inLib: filepath.Join(root, "Book"), // absolute, as every junction's target is
		out:   outside,
		loop:  loop,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unsupported here: %v", err)
		}
	}
	placeholder := filepath.Join(root, "Book", "cloud.mp3")
	if err := os.WriteFile(placeholder, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lstat := junctionLooks(map[string]bool{inLib: true, out: true, loop: true}, map[string]bool{placeholder: true})
	walk := func(p string) (string, error) { return walkLinks(p, lstat, os.Readlink) }

	got, err := walk(filepath.Join(inLib, "01.mp3"))
	if want := filepath.Join(root, "Book", "01.mp3"); err != nil || got != want {
		t.Fatalf("a file below a junction inside the library = %q, %v; want %s", got, err, want)
	}
	if rel, ok := relBelow(root, got); !ok || rel != filepath.Join("Book", "01.mp3") {
		t.Fatalf("the resolved file is %q under the root (%v), want Book/01.mp3", rel, ok)
	}
	got, err = walk(filepath.Join(out, "01.mp3"))
	if want := filepath.Join(outside, "01.mp3"); err != nil || got != want {
		t.Fatalf("a file below a junction out of the library = %q, %v; want %s", got, err, want)
	}
	if _, ok := relBelow(root, got); ok {
		t.Fatalf("a junction out of the library resolved under its root: %s", got)
	}
	if got, err := walk(placeholder); err != nil || got != placeholder {
		t.Fatalf("a reparse point that is no link = %q, %v; want it as a plain name", got, err)
	}
	// Go on Windows reads a reparse point that is neither a link nor a
	// junction as ENOENT (os.Readlink), not EINVAL: a plain name too.
	enoent := func(p string) (string, error) {
		if p == placeholder {
			return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.ENOENT}
		}
		return os.Readlink(p)
	}
	if got, err := walkLinks(placeholder, lstat, enoent); err != nil || got != placeholder {
		t.Fatalf("a reparse point Go reads as ENOENT = %q, %v; want it as a plain name", got, err)
	}
	if _, err := walk(filepath.Join(placeholder, "x")); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("a path through a file = %v, want ENOTDIR", err)
	}
	if _, err := walk(filepath.Join(loop, "x")); !errors.Is(err, errLinkLoop) {
		t.Fatalf("a junction to itself = %v, want errLinkLoop", err)
	}
	if _, err := walk(filepath.Join(root, "Book", "gone.mp3")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a name not there = %v, want ErrNotExist", err)
	}
}

// TestWalkLinksIsEvalSymlinks: with no junction, walkLinks resolves as
// filepath.EvalSymlinks does: relative and absolute links, a relative one
// climbing out and back, "." and "..".
func TestWalkLinksIsEvalSymlinks(t *testing.T) {
	t.Parallel()
	root, _ := belowFixture(t)
	for link, target := range map[string]string{
		filepath.Join(root, "Rel"):  "Book",
		filepath.Join(root, "Abs"):  filepath.Join(root, "Book"),
		filepath.Join(root, "Back"): filepath.Join("..", filepath.Base(root), "Rel"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unsupported here: %v", err)
		}
	}
	for _, p := range []string{
		filepath.Join(root, "Rel", "01.mp3"),
		filepath.Join(root, "Abs", "01.mp3"),
		filepath.Join(root, "Back", "01.mp3"),
		filepath.Join(root, "Book", ".", "..", "Rel", "01.mp3"),
	} {
		want, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := walkLinks(p, os.Lstat, os.Readlink); err != nil || got != want {
			t.Errorf("walkLinks(%s) = %q, %v; want %q as EvalSymlinks", p, got, err, want)
		}
	}
}
