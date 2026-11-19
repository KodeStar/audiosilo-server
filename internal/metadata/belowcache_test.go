package metadata

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func readBelowFolder(t *testing.T, root, path string) (string, error) {
	t.Helper()
	f, err := OpenBelow(root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

// TestBelowFolderNeverLeavesTheRoot (security): a scan's open, stat and
// lstat through a kept folder (BelowFolder) never reach a file out of the
// library: a folder swapped for a link out of the root after the path was
// checked is refused when it was not kept, and a kept one answers with its
// own file, never the link's. Allowed: a plain file, and one through a link
// that stays in the root by a relative target (one beside its folder
// included, which the folder's own root refuses) or an absolute one.
func TestBelowFolderNeverLeavesTheRoot(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	if err := os.MkdirAll(filepath.Join(root, "Shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Shared", "x.mp3"), []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, to := range map[string]string{
		filepath.Join(root, "Book", "beside.mp3"): filepath.Join("..", "Shared", "x.mp3"),
		filepath.Join(root, "Book", "abs.mp3"):    filepath.Join(root, "Shared", "x.mp3"),
		filepath.Join(root, "Book", "out.mp3"):    filepath.Join(outside, "01.mp3"),
	} {
		if err := os.Symlink(to, link); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	for name, want := range map[string]string{"01.mp3": "inside", "beside.mp3": "shared", "abs.mp3": "shared"} {
		if got, err := readBelowFolder(t, root, filepath.Join(root, "Book", name)); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, want)
		}
		if info, err := StatBelow(root, filepath.Join(root, "Book", name)); err != nil || info.Size() != int64(len(want)) {
			t.Errorf("stat %s: %v, %v", name, info, err)
		}
	}
	if got, err := readBelowFolder(t, root, filepath.Join(root, "Book", "out.mp3")); err == nil {
		t.Errorf("a link out of the root: read %q, want it refused", got)
	}
	if info, err := StatBelow(root, filepath.Join(root, "Book", "out.mp3")); err == nil {
		t.Errorf("a link out of the root: stat %v, want it refused", info)
	}
	if info, err := LstatBelow(root, filepath.Join(root, "Book", "out.mp3")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("lstat of a link out of the root: %v, %v; want the link itself", info, err)
	}

	// A folder never kept, swapped for a link out of the root: refused.
	other := filepath.Join(root, "Other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	swapForLink(t, other, outside)
	if got, err := readBelowFolder(t, root, filepath.Join(other, "01.mp3")); err == nil {
		t.Errorf("a folder swapped for a link out of the root: read %q, want it refused", got)
	}
	if info, err := StatBelow(root, filepath.Join(other, "01.mp3")); err == nil {
		t.Errorf("a folder swapped for a link out of the root: stat %v, want it refused", info)
	}

	// The kept folder (Book, read above), swapped: its own file or none,
	// never the link's.
	swapForLink(t, filepath.Join(root, "Book"), outside)
	if b, err := os.ReadFile(file); err != nil || string(b) != "outside" {
		t.Fatalf("a plain open after the swap: %q, %v; the fixture wants it to follow the link", b, err)
	}
	if got, err := readBelowFolder(t, root, file); err == nil && got != "inside" {
		t.Errorf("a kept folder swapped for a link out of the root: read %q, want its own file or none", got)
	}
}

// TestBelowFolderOpensAReplacedFolderAgain: a kept folder deleted and made
// again (a book re-copied) answers for the new folder, never "not there".
func TestBelowFolderOpensAReplacedFolderAgain(t *testing.T) {
	t.Parallel()
	root, _ := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	if got, err := readBelowFolder(t, root, file); err != nil || got != "inside" {
		t.Fatalf("first read: %q, %v", got, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "Book")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readBelowFolder(t, root, file); err != nil || got != "again" {
		t.Errorf("after the folder was replaced: %q, %v; want the new file", got, err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := readBelowFolder(t, root, file); !NotExist(err) {
		t.Errorf("a file gone: %v, want not there", err)
	}
}

// TestBelowFolderReadsTheFolderAtItsPathNow: a kept folder renamed away and
// another made at its path (the Bridge swapping a book's folder, an admin's
// rename) is never read through the old handle: the read answers for the
// folder at the path now, and with none there, not there. Allowed: the old
// folder read at its new path.
func TestBelowFolderReadsTheFolderAtItsPathNow(t *testing.T) {
	t.Parallel()
	root, _ := belowFixture(t)
	book := filepath.Join(root, "Book")
	file := filepath.Join(book, "01.mp3")
	if got, err := readBelowFolder(t, root, file); err != nil || got != "inside" {
		t.Fatalf("first read: %q, %v", got, err)
	}
	if err := os.Rename(book, filepath.Join(root, "Old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(book, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readBelowFolder(t, root, file); err != nil || got != "new" {
		t.Errorf("after the folder was replaced: %q, %v; want the new folder's file", got, err)
	}
	if info, err := StatBelow(root, file); err != nil || info.Size() != int64(len("new")) {
		t.Errorf("stat after the folder was replaced: %v, %v; want the new folder's file", info, err)
	}
	if got, err := readBelowFolder(t, root, filepath.Join(root, "Old", "01.mp3")); err != nil || got != "inside" {
		t.Errorf("the old folder at its new path: %q, %v", got, err)
	}
	if err := os.Rename(book, filepath.Join(root, "Gone")); err != nil {
		t.Fatal(err)
	}
	if got, err := readBelowFolder(t, root, file); !NotExist(err) {
		t.Errorf("a folder renamed away: read %q, %v; want not there", got, err)
	}
}

// TestFolderCacheOpenOnlyKeepsNothing: a cache that keeps no folder past its
// read (Windows, where an open folder blocks renaming the folders above it)
// closes each one when its read puts it back.
func TestFolderCacheOpenOnlyKeepsNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &folderCache{m: map[folderKey]*keptFolder{}, openOnly: true}
	f, err := c.get(root, "Book")
	if err != nil {
		t.Fatal(err)
	}
	c.put(f)
	if n := len(c.m); n != 0 {
		t.Errorf("%d folders kept, want none", n)
	}
	if _, err := f.r.Stat("."); err == nil {
		t.Error("the folder read through is still open")
	}
}

// keptNow is the folder belowFolders keeps for rel under root, nil for none.
func keptNow(root, rel string) *keptFolder {
	belowFolders.mu.Lock()
	defer belowFolders.mu.Unlock()
	return belowFolders.m[folderKey{root, rel}]
}

// TestBelowFolderKeepsItsFoldersForANameNotThere: a name not there, looked
// for again and again through a kept folder still at its path (an episode
// with no chapters file), is answered there, neither that folder nor the
// root's let go, so the next look opens neither again. Allowed: the name
// made since is found. Not parallel: it reads the shared folder cache.
func TestBelowFolderKeepsItsFoldersForANameNotThere(t *testing.T) {
	root, _ := belowFixture(t)
	book := filepath.Join(root, "Book")
	if _, err := StatBelow(root, filepath.Join(book, "01.mp3")); err != nil {
		t.Fatal(err)
	}
	kept, top := keptNow(root, "Book"), keptNow(root, ".")
	if kept == nil || top == nil {
		t.Fatalf("the folders read through are not kept: %v, %v", kept, top)
	}
	for range 3 {
		if info, err := LstatBelow(root, filepath.Join(book, "none.json")); !NotExist(err) {
			t.Fatalf("a name not there: %v, %v", info, err)
		}
	}
	if keptNow(root, "Book") != kept || keptNow(root, ".") != top {
		t.Error("a name not there let a folder still at its path go")
	}
	if err := os.WriteFile(filepath.Join(book, "none.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LstatBelow(root, filepath.Join(book, "none.json")); err != nil {
		t.Errorf("the name made since: %v", err)
	}
}

// TestLstatBelowIsTheLinksOwn: an lstat of a link reached through a folder
// os.Root refuses (an absolute target inside the root, which Below resolves)
// answers for the link itself, never for the file it leads to, as os.Lstat
// does.
func TestLstatBelowIsTheLinksOwn(t *testing.T) {
	t.Parallel()
	root, _ := belowFixture(t)
	if err := os.Symlink("01.mp3", filepath.Join(root, "Book", "link.mp3")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "Book"), filepath.Join(root, "Abs")); err != nil {
		t.Fatal(err)
	}
	info, err := LstatBelow(root, filepath.Join(root, "Abs", "link.mp3"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("lstat through an absolute link inside the root: %v, %v; want the link itself", info, err)
	}
	if info, err := StatBelow(root, filepath.Join(root, "Abs", "link.mp3")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("stat through an absolute link inside the root: %v, %v; want the file it leads to", info, err)
	}
}

// TestToolInputWithNoTempFolder (Unix): with no temporary folder to make its
// link in (a read-only one), a tool is handed the descriptor's own name and
// still reads the file opened. Not parallel: it sets TMPDIR.
func TestToolInputWithNoTempFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows hands a tool a resolved path")
	}
	root, _ := belowFixture(t)
	t.Setenv("TMPDIR", filepath.Join(root, "missing"))
	file := filepath.Join(root, "Book", "01.mp3")
	f, _, err := OpenRegularBelow(root, file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	input, extra, cleanup, err := ToolInput(f, root, file)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := exec.Command("sh", "-c", `cat "$1"`, "sh", input)
	cmd.ExtraFiles = extra
	if out, err := cmd.Output(); err != nil || string(out) != "inside" {
		t.Errorf("the tool read %q, %v (input %q); want the file opened", out, err, input)
	}
}

// A folder cache keeps at most maxFolders folders, the one idle longest
// closed for a new one, and hands the same folder to every read of it.
func TestFolderCacheIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := &folderCache{m: map[folderKey]*keptFolder{}}
	t.Cleanup(c.closeAll)
	var first *keptFolder
	for i := range maxFolders + 5 {
		rel := fmt.Sprintf("d%02d", i)
		if err := os.Mkdir(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := c.get(root, rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if i == 0 {
			first = f
		}
		c.put(f)
	}
	if n := len(c.m); n > maxFolders {
		t.Errorf("%d folders kept, want at most %d", n, maxFolders)
	}
	if _, kept := c.m[folderKey{root, "d00"}]; kept {
		t.Error("the folder idle longest is still kept")
	}
	if _, err := first.r.Stat("."); err == nil {
		t.Error("the folder let go of is still open")
	}
	last := fmt.Sprintf("d%02d", maxFolders+4)
	a, _ := c.get(root, last)
	b, _ := c.get(root, last)
	if a != b {
		t.Error("a kept folder opened again")
	}
	c.put(a)
	c.put(b)
}

// TestToolInputIsTheOpenedFile (security, Unix): a tool handed ToolInput's
// input reads the file the server opened, even once a link out of the
// library has been swapped in for its folder, and its name keeps the file's
// extension (ffmpeg guesses a format by it).
func TestToolInputIsTheOpenedFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows hands a tool a resolved path")
	}
	root, outside := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	f, _, err := OpenRegularBelow(root, file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	swapForLink(t, filepath.Join(root, "Book"), outside)
	input, extra, cleanup, err := ToolInput(f, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(input) != ".mp3" {
		t.Errorf("input %q, want the file's extension", input)
	}
	cmd := exec.Command("sh", "-c", `cat "$1"`, "sh", input)
	cmd.ExtraFiles = extra
	out, err := cmd.Output()
	cleanup()
	if err != nil || string(out) != "inside" {
		t.Errorf("the tool read %q, %v; want the file opened", out, err)
	}
	if _, err := os.Lstat(input); err == nil {
		t.Error("the input's link is left once the tool is done")
	}
}

// BenchmarkOpenBelowFolder is a scan's open of a book's file (openBlockFile's)
// through its kept folder, against the plain open of the path it replaced
// (BenchmarkOpenPlain) and a walk from the root for each file
// (BenchmarkOpenBelowWalk).
func BenchmarkOpenBelowFolder(b *testing.B) {
	root, file := benchFile(b)
	for b.Loop() {
		f, _, err := openRegularWith(file, func() (*os.File, error) { return openBelow(root, file) })
		if err != nil {
			b.Fatal(err)
		}
		_ = f.Close()
	}
}

func BenchmarkOpenBelowWalk(b *testing.B) {
	root, file := benchFile(b)
	for b.Loop() {
		f, _, err := OpenRegularBelow(root, file)
		if err != nil {
			b.Fatal(err)
		}
		_ = f.Close()
	}
}

func BenchmarkOpenPlain(b *testing.B) {
	_, file := benchFile(b)
	for b.Loop() {
		f, _, err := openRegular(file)
		if err != nil {
			b.Fatal(err)
		}
		_ = f.Close()
	}
}

func benchFile(b *testing.B) (root, file string) {
	b.Helper()
	root = b.TempDir()
	file = filepath.Join(root, "Author", "Series", "Book", "01.mp3")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		b.Fatal(err)
	}
	return root, file
}

// closeAll closes every folder kept and no read holds (a test's cleanup).
func (c *folderCache) closeAll() {
	c.mu.Lock()
	var closing []*keptFolder
	for k, f := range c.m {
		if f.refs == 0 {
			delete(c.m, k)
			closing = append(closing, f)
		}
	}
	c.mu.Unlock()
	closeFolders(closing)
}
