package metadata

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// belowFixture is a root holding Book/01.mp3 and a folder outside it holding
// a file of the same name, both resolved (no link above either).
func belowFixture(t *testing.T) (root, outside string) {
	t.Helper()
	var err error
	if root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if outside, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(root, "Book", "01.mp3"): "inside",
		filepath.Join(outside, "01.mp3"):      "outside",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// swapForLink moves the folder at dir aside and puts a link to target in its
// place: what changes on disk between a path's check and its open.
func swapForLink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+".aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
}

func readBelow(t *testing.T, root, path string) (string, error) {
	t.Helper()
	f, _, err := OpenRegularBelow(root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

// TestOpenRegularBelowNeverLeavesTheRoot (security): a file opened through
// its library's root is never one out of it, whatever its path leads to by
// the time it is opened: a folder swapped for a link out of the root after
// the path was checked (a plain open of the same path reads the file there)
// is refused, and so is a link out of the root at the file itself. Allowed:
// a plain file, and one through a link that stays in the root, by a relative
// target, by an absolute one, and through a root that is itself a link,
// named as configured or as resolved.
func TestOpenRegularBelowNeverLeavesTheRoot(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	if got, err := readBelow(t, root, file); err != nil || got != "inside" {
		t.Fatalf("a plain file: %q, %v", got, err)
	}
	for link, to := range map[string]string{"Rel": "Book", "Abs": filepath.Join(root, "Book"), "Out": outside} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	for _, link := range []string{"Rel", "Abs"} {
		if got, err := readBelow(t, root, filepath.Join(root, link, "01.mp3")); err != nil || got != "inside" {
			t.Errorf("through the link %s, which stays in the root: %q, %v; want the file", link, got, err)
		}
	}
	if got, err := readBelow(t, root, filepath.Join(root, "Out", "01.mp3")); err == nil {
		t.Errorf("through a link out of the root: read %q, want it refused", got)
	}
	// A root that is itself a link: the path joined on it as configured, and
	// as a join returns it (on the resolved root).
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(alias, "Book", "01.mp3"), file, filepath.Join(root, "Abs", "01.mp3")} {
		if got, err := readBelow(t, alias, path); err != nil || got != "inside" {
			t.Errorf("under a root that is a link, %s: %q, %v; want the file", path, got, err)
		}
	}
	if _, err := readBelow(t, root, filepath.Join(outside, "01.mp3")); err == nil {
		t.Error("a path that is not under the root opened")
	}
	if _, err := readBelow(t, root, file+"\x00"); err == nil {
		t.Error("a path holding a NUL opened")
	}

	// The swap: the path was checked while Book was a folder; by the open it
	// is a link out of the root.
	swapForLink(t, filepath.Join(root, "Book"), outside)
	if b, err := os.ReadFile(file); err != nil || string(b) != "outside" {
		t.Fatalf("a plain open after the swap: %q, %v; the fixture wants it to follow the link", b, err)
	}
	if got, err := readBelow(t, root, file); err == nil {
		t.Errorf("after its folder was swapped for a link out of the root: read %q, want it refused", got)
	}
}

// TestReadDirBelowNeverLeavesTheRoot (security): a folder listed through its
// library's root is never one out of it: one swapped for a link out of the
// root after its path was checked is refused, where a plain listing of the
// path lists the folder the link points to, and no entry's info is read by
// the path again. Allowed: a folder in the root lists, sorted, with its
// entries' info, through a link that stays in the root too.
func TestReadDirBelowNeverLeavesTheRoot(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	dir := filepath.Join(root, "Book")
	if err := os.WriteFile(filepath.Join(dir, "00.mp3"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(root, "Abs")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	for _, path := range []string{dir, filepath.Join(root, "Abs"), root} {
		d, err := ReadDirBelowBounded(t.Context(), root, path, 0)
		if err != nil {
			t.Fatalf("listing %s: %v", path, err)
		}
		if path != root {
			if len(d.Entries) != 2 || d.Entries[0].Name() != "00.mp3" || d.Entries[1].Name() != "01.mp3" {
				t.Errorf("listing %s: %v, want 00.mp3 then 01.mp3", path, d.Entries)
			} else if fi, err := d.Info(d.Entries[0]); err != nil || fi.Size() != int64(len("first")) {
				t.Errorf("the info of %s's first entry: %v, %v", path, fi, err)
			}
		}
		_ = d.Close()
	}

	d, err := ReadDirBelow(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	swapForLink(t, dir, outside)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("a plain listing after the swap: %v, %v; the fixture wants it to follow the link", entries, err)
	}
	if again, err := ReadDirBelow(root, dir); err == nil {
		_ = again.Close()
		t.Errorf("after the folder was swapped for a link out of the root: listed %v, want it refused", again.Entries)
	}
	// The folder opened before the swap still answers for its own entries:
	// 01.mp3 there is "inside" (6 bytes), the link's is "outside" (7).
	for _, e := range d.Entries {
		if e.Name() != "01.mp3" {
			continue
		}
		if fi, err := d.Info(e); err != nil || fi.Size() != int64(len("inside")) {
			t.Errorf("an entry's info after the swap: %v, %v; want the listed folder's own file", fi, err)
		}
	}
}
