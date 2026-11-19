package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// linkOutEnv is a library whose book folder Author/Book holds a real part
// (01.m4b), a link to a part inside the library (02.m4b -> Shared/in.m4b), and
// what a test puts there; outside is a folder out of the root with an audio
// file (secret.m4b), an image (secret.jpg) and a book folder (Secret Book).
// reads is every file the last scan's reads were handed (Scanner.readFile).
type linkOutEnv struct {
	cat           *catalog.Catalog
	s             *Scanner
	ctx           context.Context
	lib           *catalog.Library
	root, outside string
	mu            sync.Mutex
	reads         []string
}

func newLinkOutEnv(t *testing.T) *linkOutEnv {
	t.Helper()
	cat, s, ctx := newScanEnv(t)
	e := &linkOutEnv{cat: cat, s: s, ctx: ctx, root: t.TempDir(), outside: t.TempDir()}
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(e.root, "Shared", "in.m4b"))
	copyFixtureM4B(t, filepath.Join(e.outside, "secret.m4b"))
	copyFixtureM4B(t, filepath.Join(e.outside, "Secret Book", "01.m4b"))
	writeFile(t, e.outside, "secret.jpg", []byte("img"))
	if err := os.Symlink(filepath.Join("..", "..", "Shared", "in.m4b"), filepath.Join(e.root, "Author", "Book", "02.m4b")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Links", Root: e.root})
	if err != nil {
		t.Fatal(err)
	}
	e.lib = lib
	s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		e.mu.Lock()
		e.reads = append(e.reads, p)
		e.mu.Unlock()
		return extractFile(ctx, root, p, ffprobe, timeout)
	}
	return e
}

func (e *linkOutEnv) link(t *testing.T, target, rel string) {
	t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// scan scans the library, reads holding only this scan's.
func (e *linkOutEnv) scan(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	e.reads = nil
	e.mu.Unlock()
	if _, err := e.s.Scan(e.ctx, *e.lib); err != nil {
		t.Fatal(err)
	}
}

// files is the indexed book's parts at rel, nil when there is no book there.
func (e *linkOutEnv) files(t *testing.T, rel string) []string {
	t.Helper()
	b, err := e.cat.GetBookByPath(e.ctx, e.lib.ID, rel)
	if err != nil {
		return nil
	}
	return filePaths(b)
}

// readThrough reports any read the scan was handed through one of the links
// out (rels), or of a file out of the root.
func (e *linkOutEnv) readThrough(rels ...string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var bad []string
	for _, p := range e.reads {
		through := slices.ContainsFunc(rels, func(rel string) bool { return p == filepath.Join(e.root, filepath.FromSlash(rel)) })
		if through || strings.HasPrefix(p, e.outside) {
			bad = append(bad, p)
		}
	}
	return bad
}

// The scan never takes a link out of the library root as a book's part: a
// file link and a folder link in a book folder (one named as audio, which the
// walk would otherwise list as a part; one named as a disc) are not indexed,
// and no read is handed either; a folder holding only a link out makes no
// book. Allowed: a link to a part inside the root is a part as ever.
func TestScanSkipsLinksOutOfTheRoot(t *testing.T) {
	t.Parallel()
	e := newLinkOutEnv(t)
	e.link(t, filepath.Join(e.outside, "secret.m4b"), "Author/Book/03.m4b")
	e.link(t, filepath.Join(e.outside, "Secret Book"), "Author/Book/04.m4b")
	e.link(t, filepath.Join(e.outside, "Secret Book"), "Author/Book/CD2")
	e.link(t, filepath.Join(e.outside, "secret.m4b"), "Author/Only Out/01.m4b")
	e.scan(t)
	if got, want := e.files(t, "Author/Book"), []string{"Author/Book/01.m4b", "Author/Book/02.m4b"}; !slices.Equal(got, want) {
		t.Errorf("the book's parts = %v, want %v (the in-root link kept, the links out never)", got, want)
	}
	if got := e.files(t, "Author/Only Out"); got != nil {
		t.Errorf("a folder of only a link out indexed %v, want no book", got)
	}
	if bad := e.readThrough("Author/Book/03.m4b", "Author/Book/04.m4b", "Author/Only Out/01.m4b"); len(bad) > 0 {
		t.Errorf("the scan read through a link out: %v", bad)
	}
	// On demand (an asked book worked out from its own folder's listing,
	// audioEntries, never the index), the same.
	b, err := e.s.resolveBook(e.ctx, *e.lib, "Author/Book/01.m4b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if parts, want := filePaths(b), []string{"Author/Book/01.m4b", "Author/Book/02.m4b"}; !slices.Equal(parts, want) {
		t.Errorf("the book resolved on demand has parts %v, want %v", parts, want)
	}
	if _, err := e.s.resolveBook(e.ctx, *e.lib, "Author/Only Out", nil); !errors.Is(err, ErrNotIndexable) {
		t.Errorf("a folder of only a link out resolved on demand: %v, want ErrNotIndexable", err)
	}
}

// A part replaced by a link out of the root since the last scan leaves the
// book on the next scan, as any part gone does, and a book left with only such
// a link is removed.
func TestScanDropsAPartNowALinkOut(t *testing.T) {
	t.Parallel()
	e := newLinkOutEnv(t)
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "Book", "03.m4b"))
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "Gone", "01.m4b"))
	e.scan(t)
	if got := e.files(t, "Author/Book"); len(got) != 3 || e.files(t, "Author/Gone") == nil {
		t.Fatalf("before: the book's parts %v, Author/Gone %v; want 3 parts and the book", got, e.files(t, "Author/Gone"))
	}
	for _, rel := range []string{"Author/Book/03.m4b", "Author/Gone/01.m4b"} {
		if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		e.link(t, filepath.Join(e.outside, "secret.m4b"), rel)
	}
	e.scan(t)
	if got, want := e.files(t, "Author/Book"), []string{"Author/Book/01.m4b", "Author/Book/02.m4b"}; !slices.Equal(got, want) {
		t.Errorf("after: the book's parts = %v, want %v", got, want)
	}
	if got := e.files(t, "Author/Gone"); got != nil {
		t.Errorf("after: a book of only a link out = %v, want it removed", got)
	}
	if bad := e.readThrough("Author/Book/03.m4b", "Author/Gone/01.m4b"); len(bad) > 0 {
		t.Errorf("the scan read through a link out: %v", bad)
	}
}

// A conventionally named cover that links out of the root is no cover, nor is
// an image listed in the book folder that does: the image inside the folder
// is the cover. Allowed: an in-root link to an image is taken.
func TestCoverSkipsLinksOutOfTheRoot(t *testing.T) {
	t.Parallel()
	e := newLinkOutEnv(t)
	e.link(t, filepath.Join(e.outside, "secret.jpg"), "Author/Book/cover.jpg")
	e.link(t, filepath.Join(e.outside, "secret.jpg"), "Author/Book/a cover.png")
	writeFile(t, e.root, "Author/Book/front.png", []byte("img"))
	writeFile(t, e.root, "Shared/art.jpg", []byte("img"))
	e.link(t, filepath.Join("..", "..", "Shared", "art.jpg"), "Author/In/folder.jpg")
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "In", "01.m4b"))
	e.scan(t)
	for rel, want := range map[string]string{"Author/Book": "Author/Book/front.png", "Author/In": "Author/In/folder.jpg"} {
		b, err := e.cat.GetBookByPath(e.ctx, e.lib.ID, rel)
		if err != nil {
			t.Fatal(err)
		}
		if b.CoverPath != want {
			t.Errorf("%s: cover %q, want %q", rel, b.CoverPath, want)
		}
	}
}

// A link whose read fails, as a share does that a retry may get past (EIO) or
// for good (a permission, or a limit of the server's own), is not known to
// stay inside the root: the book is kept as stored, never built without the
// part nor with it, nor removed when the link was its only other part; an
// asked book hears a look given up on (the API's 503) for the first, a look
// refused for good (lookRefused: a 404) for the other. Not parallel: it swaps
// SafeJoin's looks.
func TestScanKeepsABookWhoseLinkDidNotAnswer(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.EACCES, syscall.ENOMEM} {
		t.Run(errno.Error(), func(t *testing.T) {
			e := newLinkOutEnv(t)
			e.scan(t)
			want := []string{"Author/Book/01.m4b", "Author/Book/02.m4b"}
			if got := e.files(t, "Author/Book"); !slices.Equal(got, want) {
				t.Fatalf("before: the book's parts = %v, want %v", got, want)
			}
			was := plainLooks
			t.Cleanup(func() { plainLooks = was })
			plainLooks = plainLooksOver(os.Lstat, func(p string) (string, error) {
				if strings.HasSuffix(p, filepath.Join("Author", "Book", "02.m4b")) {
					return "", &fs.PathError{Op: "readlink", Path: p, Err: errno}
				}
				return os.Readlink(p)
			})
			if err := os.Remove(filepath.Join(e.root, "Author", "Book", "01.m4b")); err != nil {
				t.Fatal(err)
			}
			e.scan(t)
			if got := e.files(t, "Author/Book"); !slices.Equal(got, want) {
				t.Errorf("after a link's look failed: the book's parts = %v, want it kept as stored %v", got, want)
			}
			_, err := e.s.resolveBook(e.ctx, *e.lib, "Author/Book", nil)
			if refused := errors.As(err, new(lookRefused)); !metadata.GaveUp(err) || refused != (errno != syscall.EIO) {
				t.Errorf("the book asked for with its link's look failing: %v (refused for good %v), want a look given up on, refused for good only past EIO", err, refused)
			}
			if bad := e.readThrough("Author/Book/02.m4b"); len(bad) > 0 {
				t.Errorf("the scan read through the link whose look failed: %v", bad)
			}
		})
	}
}

// A book folder whose only image is a link out of the root has no cover
// (coverLook.find), and the walk doesn't count that image as one put beside
// it (coversOfWalk), so an unchanged scan doesn't read the book again for it.
// Allowed: an image linked from inside the root, added later, is read as
// the book's cover on the next scan.
func TestScanIgnoresAnImageLinkOutOfTheRoot(t *testing.T) {
	t.Parallel()
	e := newLinkOutEnv(t)
	copyFixtureM4B(t, filepath.Join(e.root, "Author", "Pic", "01.m4b"))
	e.link(t, filepath.Join(e.outside, "secret.jpg"), "Author/Pic/cover.jpg")
	e.link(t, filepath.Join(e.outside, "secret.jpg"), "Author/Pic/Back.png")
	e.scan(t)
	e.scan(t)
	e.mu.Lock()
	again := slices.ContainsFunc(e.reads, func(p string) bool { return strings.Contains(p, filepath.Join("Author", "Pic")) })
	e.mu.Unlock()
	if again {
		t.Errorf("an unchanged book with only an image link out of the root was read again: %v", e.reads)
	}
	writeFile(t, e.root, "Shared/art.jpg", []byte("img"))
	e.link(t, filepath.Join("..", "..", "Shared", "art.jpg"), "Author/Pic/art.jpg")
	e.scan(t)
	b, err := e.cat.GetBookByPath(e.ctx, e.lib.ID, "Author/Pic")
	if err != nil {
		t.Fatal(err)
	}
	if b.CoverPath != "Author/Pic/art.jpg" {
		t.Errorf("cover once an image linked from inside the root is added = %q, want Author/Pic/art.jpg", b.CoverPath)
	}
}

// The browse view's pending-book check (hasAudio) counts only the audio a scan
// indexes: a folder of only a link out of the root is no book to come, a
// folder with a link inside it is.
func TestHasAudioSkipsLinksOutOfTheRoot(t *testing.T) {
	t.Parallel()
	e := newLinkOutEnv(t)
	e.link(t, filepath.Join(e.outside, "secret.m4b"), "Author/Only Out/01.m4b")
	e.link(t, filepath.Join("..", "..", "Shared", "in.m4b"), "Author/Only In/01.m4b")
	if hasAudio(e.ctx, e.root, "Author/Only Out", nil) {
		t.Error("hasAudio counts a link out of the root")
	}
	if !hasAudio(e.ctx, e.root, "Author/Only In", nil) {
		t.Error("hasAudio misses a link inside the root")
	}
}

// A conventionally named cover that is a link out of the root is never looked
// at through the link (no stat of its target): its own entry says it is a
// link, and the link leads out. Not parallel: it swaps the cover's looks.
func TestCoverNeverStatsALinkOutOfTheRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, outside, "secret.jpg", []byte("img"))
	writeFile(t, root, "Book/01.m4b", []byte("x"))
	if err := os.Symlink(filepath.Join(outside, "secret.jpg"), filepath.Join(root, "Book", "cover.jpg")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	var mu sync.Mutex
	var stats []string
	was := statLook
	t.Cleanup(func() { statLook = was })
	statLook = func(ctx context.Context, root, p string, timeout time.Duration) (fs.FileInfo, error) {
		mu.Lock()
		stats = append(stats, p)
		mu.Unlock()
		return was(ctx, root, p, timeout)
	}
	var g givenUp
	if got := (coverLook{ctx: t.Context(), root: root, timeout: time.Minute, gaveUp: &g}).find(filepath.Join(root, "Book"), false); got != "" || g.err != nil {
		t.Errorf("cover %q, given up on %v; want none", got, g.err)
	}
	if slices.Contains(stats, filepath.Join(root, "Book", "cover.jpg")) {
		t.Errorf("the cover link out of the root was stat'ed: %v", stats)
	}
}
