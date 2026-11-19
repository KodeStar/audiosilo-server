package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/testwait"
)

// belowEnv is a library (root, resolved: no link above it) whose book folder
// Author/Book holds a part that is no audio ("inside"), and a folder out of
// the root (outside) holding a real audiobook of the same name, whose tags
// (title Unsouled, 2 s) are what a read through a swapped-in link would
// index.
type belowEnv struct {
	cat           *catalog.Catalog
	s             *Scanner
	ctx           context.Context
	lib           *catalog.Library
	root, outside string
}

func newBelowEnv(t *testing.T) *belowEnv {
	t.Helper()
	cat, s, ctx := newScanEnv(t)
	e := &belowEnv{cat: cat, s: s, ctx: ctx}
	var err error
	if e.root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if e.outside, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.root, "Author/Book/01.m4b", []byte("inside"))
	copyFixtureM4B(t, filepath.Join(e.outside, "01.m4b"))
	if e.lib, err = cat.CreateLibrary(ctx, catalog.Library{Name: "Below", Root: e.root}); err != nil {
		t.Fatal(err)
	}
	return e
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

// swapWhileHeld runs do with every open under dir held (its looks answer:
// metadata.HoldReadsForTests), swaps dir for a link to target once do has
// checked its path and waits on an open, then lets the opens go and waits
// for do. Not for a parallel test (the hold swaps the mount lookup).
func swapWhileHeld(t *testing.T, dir, target string, do func()) {
	t.Helper()
	release := metadata.HoldReadsForTests(dir)
	t.Cleanup(release)
	done := make(chan struct{})
	go func() {
		defer close(done)
		do()
	}()
	time.Sleep(testwait.Scaled(500 * time.Millisecond))
	swapForLink(t, dir, target)
	release()
	<-done
}

// noOutsideRead fails the test when the book at rel was indexed from the
// file out of the root: its tags, its length or its fingerprint.
func (e *belowEnv) noOutsideRead(t *testing.T, rel, what string) {
	t.Helper()
	b, err := e.cat.GetBookByPath(e.ctx, e.lib.ID, rel)
	if errors.Is(err, catalog.ErrNotFound) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(e.outside, "01.m4b")
	if strings.Contains(b.Title, "Unsouled") || b.Duration == 2 || b.ContentHash == metadata.Fingerprint(e.ctx, e.outside, out, time.Minute) {
		t.Errorf("%s: the book = %q, %v s, fingerprint %q: read from the file out of the library", what, b.Title, b.Duration, b.ContentHash)
	}
}

// TestIndexPathReadsThroughTheRootAfterASwap (security): a book read on
// demand (IndexPath: resolveBook's looks, then its tags, probe and
// fingerprint) whose folder is swapped for a link out of the library after
// every look, as its first file is read, never reads the file the link
// leads to. Not parallel: the swap is in the scanner's read.
func TestIndexPathReadsThroughTheRootAfterASwap(t *testing.T) {
	e := newBelowEnv(t)
	var once sync.Once
	e.s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		once.Do(func() { swapForLink(t, filepath.Join(e.root, "Author", "Book"), e.outside) })
		return extractFile(ctx, root, p, ffprobe, timeout)
	}
	_, _ = e.s.IndexPath(e.ctx, *e.lib, "Author/Book")
	e.noOutsideRead(t, "Author/Book", "read on demand after the swap")
}

// TestScanReadsThroughTheRootAfterASwap (security): a scan whose walk listed
// a book folder that is swapped for a link out of the library before its
// files are read never reads the file the link leads to. Allowed: the same
// scan of the folder as it was reads its part.
func TestScanReadsThroughTheRootAfterASwap(t *testing.T) {
	e := newBelowEnv(t)
	copyFixtureM4B(t, filepath.Join(e.root, "Other", "Book", "01.m4b"))
	swapWhileHeld(t, filepath.Join(e.root, "Author", "Book"), e.outside, func() {
		_, _ = e.s.Scan(e.ctx, *e.lib)
	})
	e.noOutsideRead(t, "Author/Book", "scanned after the swap")
	if b, err := e.cat.GetBookByPath(e.ctx, e.lib.ID, "Other/Book"); err != nil || b.Duration != 2 {
		t.Errorf("the book left alone: %+v, %v; want it read", b, err)
	}
}

// TestFFprobeReadsTheFileTheScanOpened (security): ffprobe is handed the
// file the scan opened below the library's root, never its path: a folder
// swapped for a link out of the library after the open (here by the
// stand-in itself, as it starts) leaves ffprobe reading the file opened, not
// the one the link leads to.
func TestFFprobeReadsTheFileTheScanOpened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stand-in is a shell script")
	}
	e := newBelowEnv(t)
	writeFile(t, e.outside, "01.m4b", []byte("outside"))
	book := filepath.Join(e.root, "Author", "Book")
	probe := stubProbe(t, `for f; do :; done
[ -L '`+book+`' ] || { mv '`+book+`' '`+book+`.aside' && ln -s '`+e.outside+`' '`+book+`'; }
c=$(head -c 7 "$f")
printf '{"streams":[{"codec_name":"mp3","codec_type":"audio"}],"format":{"duration":"3.0","tags":{"album":"%s","title":"%s"}}}' "$c" "$c"`)
	s := NewScanner(e.cat, probe, e.s.log)
	b, err := s.IndexPath(e.ctx, *e.lib, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.Title, "inside") || b.Duration != 3 {
		t.Errorf("the book ffprobe read = %q, %v s; want the file opened (%q), never the one through the link", b.Title, b.Duration, "inside")
	}
}

// TestAttachmentsReadThroughTheRootAfterASwap (security): a book's
// .audiosilo folder swapped for a link out of the library after the walk
// listed it is never read through: none of the files there is indexed.
// Allowed: the attachment as the walk found it is read by a scan with no
// swap.
func TestAttachmentsReadThroughTheRootAfterASwap(t *testing.T) {
	e := newBelowEnv(t)
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "Book", "01.m4b"))
	past := time.Now().Add(-time.Hour)
	writeAt(t, e.root, "Author/Book/.audiosilo/description.json", testDescription, past)
	out := strings.Replace(testDescription, `"work":"w"`, `"work":"out"`, 1)
	writeAt(t, e.outside, ".audiosilo/description.json", out, past)
	swapWhileHeld(t, filepath.Join(e.root, "Author", "Book", ".audiosilo"), filepath.Join(e.outside, ".audiosilo"), func() {
		_, _ = e.s.Scan(e.ctx, *e.lib)
	})
	if a := attachmentsAt(t, e.cat, e.lib.ID, "Author/Book"); a != nil && a.Description != nil && a.Description.Work == "out" {
		t.Errorf("the description indexed after the swap is the one out of the library: %+v", a.Description)
	}

	// No swap: the folder put back is read.
	dir := filepath.Join(e.root, "Author", "Book", ".audiosilo")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".aside", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Scan(e.ctx, *e.lib); err != nil {
		t.Fatal(err)
	}
	if a := attachmentsAt(t, e.cat, e.lib.ID, "Author/Book"); a == nil || a.Description == nil || a.Description.Work != "w" {
		t.Errorf("the description with no swap = %+v, want the book's own", a)
	}
}

// TestReadalongMapReadThroughTheRootAfterASwap (security): a book's
// .audiosilo folder swapped for a link out of the library after
// ReadReadalongMap looked at it is never read through. Allowed: the map
// before the swap. Not parallel: it swaps readalongLstat.
func TestReadalongMapReadThroughTheRootAfterASwap(t *testing.T) {
	e := newBelowEnv(t)
	body, out := testReadalong(strings.Repeat("a", 64)), testReadalong(strings.Repeat("b", 64))
	writeFile(t, e.root, "Author/Book/.audiosilo/readalong.json", []byte(body))
	writeFile(t, e.outside, ".audiosilo/readalong.json", []byte(out))
	if data, _, err := ReadReadalongMap(*e.lib, "Author/Book", true); err != nil || string(data) != body {
		t.Fatalf("before the swap: %q, %v; want the book's map", data, err)
	}
	was := readalongLstat
	t.Cleanup(func() { readalongLstat = was })
	readalongLstat = func(root, p string) (fs.FileInfo, error) {
		info, err := was(root, p)
		swapForLink(t, p, filepath.Join(e.outside, ".audiosilo"))
		return info, err
	}
	// The folder opened before the swap (metadata.BelowFolder keeps it a
	// moment) may still answer with its own map; the one through the link,
	// never.
	if data, _, err := ReadReadalongMap(*e.lib, "Author/Book", true); string(data) == out {
		t.Errorf("after the swap: %q, %v; want never the map out of the library", data, err)
	}
}

// TestLookArtStatsThroughTheRootAfterASwap (security): a cover whose folder
// is swapped for a link out of the library after LookArt joined its path is
// never stat'ed through it: no version of the file there. Allowed: the
// cover in place. Not parallel: it swaps artStat.
func TestLookArtStatsThroughTheRootAfterASwap(t *testing.T) {
	e := newBelowEnv(t)
	writeFile(t, e.root, "Author/Book/cover.jpg", []byte("in"))
	writeFile(t, e.outside, "cover.jpg", []byte("a larger image out of the library"))
	src := catalog.CoverSource{CoverPath: "Author/Book/cover.jpg", AudioPath: "Author/Book/01.m4b"}
	if art, err := LookArt(e.ctx, e.root, src, time.Minute); err != nil || art.Info.Size() != 2 {
		t.Fatalf("before the swap: %+v, %v; want the cover", art, err)
	}
	was := artStat
	t.Cleanup(func() { artStat = was })
	artStat = func(root, p string) (fs.FileInfo, error) {
		swapForLink(t, filepath.Join(e.root, "Author", "Book"), e.outside)
		return was(root, p)
	}
	art, err := LookArt(e.ctx, e.root, src, time.Minute)
	if err == nil && art.Info.Size() != 2 {
		t.Errorf("after the swap: %d bytes; want no stat through the link out of the library", art.Info.Size())
	}
}
