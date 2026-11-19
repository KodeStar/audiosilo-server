//go:build unix

package metadata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A start removes the folders ToolInput left behind by an earlier run, and
// nothing else in the temporary folder: not one in use (new), not another
// user's, not one holding anything but its link, not a link named like one,
// not a folder with another mode or name.
func TestSweepInputDirsRemovesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	uid := uint32(os.Getuid()) //nolint:gosec // G115: a uid is never negative on Unix
	old := time.Now().Add(-2 * inputDirStale)
	mk := func(name string, perm os.FileMode, link string, at time.Time) string {
		t.Helper()
		dir := filepath.Join(tmp, name)
		if err := os.Mkdir(dir, perm); err != nil {
			t.Fatal(err)
		}
		if link != "" {
			if err := os.Symlink(inheritedInput, filepath.Join(dir, link)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	left := mk(inputDirPrefix+"111", 0o700, "input.mp3", old)
	leftEmpty := mk(inputDirPrefix+"222", 0o700, "", old)
	inUse := mk(inputDirPrefix+"333", 0o700, "input.m4b", time.Now())
	otherMode := mk(inputDirPrefix+"444", 0o755, "input.mp3", old)
	otherName := mk("audiosilo-inputs-555", 0o700, "input.mp3", old)
	otherLink := mk(inputDirPrefix+"666", 0o700, "notes.txt", old)
	withFile := filepath.Join(tmp, inputDirPrefix+"777")
	if err := os.Mkdir(withFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withFile, "input.mp3"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(withFile, old, old); err != nil {
		t.Fatal(err)
	}
	// A link named like one, to a folder that looks like one.
	target := mk("elsewhere", 0o700, "input.mp3", old)
	linked := filepath.Join(tmp, inputDirPrefix+"888")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}

	if n := sweepInputDirs(tmp, uid+1, time.Now().Add(-inputDirStale)); n != 0 {
		t.Fatalf("%d folders of another user's removed, want none", n)
	}
	if n := sweepInputDirs(tmp, uid, time.Now().Add(-inputDirStale)); n != 2 {
		t.Errorf("%d folders removed, want the two an earlier run left", n)
	}
	for _, gone := range []string{left, leftEmpty} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", filepath.Base(gone), err)
		}
	}
	for _, kept := range []string{inUse, otherMode, otherName, otherLink, withFile, target, linked,
		filepath.Join(inUse, "input.m4b"), filepath.Join(withFile, "input.mp3"), filepath.Join(target, "input.mp3")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

// The folder a tool's input link is made in is one a later start's sweep
// knows for its own once it is old.
func TestSweepInputDirsKnowsInputLinks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	link, cleanup, err := inputLink("/books/A/01.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	uid := uint32(os.Getuid()) //nolint:gosec // G115: a uid is never negative on Unix
	if n := sweepInputDirs(tmp, uid, time.Now().Add(-inputDirStale)); n != 0 {
		t.Fatalf("a folder in use was removed (%d)", n)
	}
	if n := sweepInputDirs(tmp, uid, time.Now().Add(time.Minute)); n != 1 {
		t.Fatalf("%d folders removed, want the one inputLink made", n)
	}
	if _, err := os.Lstat(filepath.Dir(link)); !os.IsNotExist(err) {
		t.Errorf("the folder is still there (%v)", err)
	}
}
