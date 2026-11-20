//go:build windows

package metadata

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mklinkJ makes a junction at link to target (cmd's mklink /J), skipping the
// test where none can be made.
func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("no junction made: %v %s", err, out)
	}
}

// TestJunctionBelowTheRoot (security, Windows): a file below a junction inside
// the library opens through the root (Below resolves the junction, which
// os.Root refuses, as its target is absolute, and EvalSymlinks no longer
// follows) and is handed to a tool by its real path; one below a junction out
// of the library is refused by both.
func TestJunctionBelowTheRoot(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	mklinkJ(t, filepath.Join(root, "Shelf"), filepath.Join(root, "Book"))
	mklinkJ(t, filepath.Join(root, "Away"), outside)
	read := func(path string) (string, error) {
		f, _, err := OpenRegularBelow(root, path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return string(b), err
	}
	in := filepath.Join(root, "Shelf", "01.mp3")
	if got, err := read(in); err != nil || got != "inside" {
		t.Fatalf("a file below a junction inside the library = %q, %v; want its bytes", got, err)
	}
	f, _, err := OpenRegularBelow(root, in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	input, _, cleanup, err := heldPathInput(f, root, in)
	if err != nil || !sameFilePath(t, input, filepath.Join(root, "Book", "01.mp3")) {
		t.Fatalf("heldPathInput through a junction inside the library = %q, %v; want the real path", input, err)
	}
	cleanup()
	if got, err := read(filepath.Join(root, "Away", "01.mp3")); err == nil {
		t.Fatalf("a file below a junction out of the library opened: %q", got)
	}
}

// sameFilePath reports whether a and b name one file.
func sameFilePath(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}
