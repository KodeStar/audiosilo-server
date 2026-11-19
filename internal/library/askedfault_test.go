package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// askedEnv is a library over a new folder holding files (each a KiB of
// nothing), its scanner (no ffprobe) and the three asks for a book: an
// on-demand index, an admin's Re-read and the Bridge's IndexMoved.
func askedEnv(t *testing.T, files ...string) (root string, lib *catalog.Library, s *Scanner, asks map[string]func(rel string) (*catalog.Book, error)) {
	t.Helper()
	ctx := t.Context()
	cat := catalog.New(storetest.Open(t), time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		writeFile(t, root, f, make([]byte, 1<<10))
	}
	lib, err = cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s = NewScanner(cat, "", slog.Default())
	asks = map[string]func(string) (*catalog.Book, error){
		"IndexPath":   func(rel string) (*catalog.Book, error) { return s.IndexPath(ctx, *lib, rel) },
		"ReindexPath": func(rel string) (*catalog.Book, error) { return s.ReindexPath(ctx, *lib, rel) },
		"IndexMoved":  func(rel string) (*catalog.Book, error) { return s.IndexMoved(ctx, *lib, rel, "") },
	}
	return root, lib, s, asks
}

// faultClasses are the share-fault rule's classes for a caller who asked for a
// book: a fault a retry may get past (true: a look given up on, the API's 503
// share_not_answering), and every other (false: the lasting answer, a 404 with
// nothing to try again): a refusal that stays (a permission, the device gone,
// a link loop, a name the share refuses) and a limit of the server's own (too
// many open files, no memory, busy), which is no share's fault.
var faultClasses = []struct {
	errno     syscall.Errno
	transient bool
}{
	{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.ETIMEDOUT, true}, {syscall.ENOTCONN, true},
	{syscall.EACCES, false}, {syscall.EPERM, false}, {syscall.ENXIO, false},
	{syscall.ELOOP, false}, {syscall.EINVAL, false},
	{syscall.EMFILE, false}, {syscall.ENFILE, false}, {syscall.ENOMEM, false}, {syscall.EAGAIN, false}, {syscall.EBUSY, false},
}

// checkAsked checks err, an ask's, against a fault's class: a transient one a
// look given up on, any other ErrNotIndexable and no look given up on.
func checkAsked(t *testing.T, what string, err error, transient bool) {
	t.Helper()
	switch {
	case transient && !metadata.GaveUp(err):
		t.Errorf("%s: %v, want a look given up on", what, err)
	case !transient && (!errors.Is(err, ErrNotIndexable) || metadata.GaveUp(err)):
		t.Errorf("%s: %v, want ErrNotIndexable, no look given up on", what, err)
	}
}

// A caller who asks for a book (an on-demand index, an admin's Re-read, the
// Bridge's IndexMoved) one of whose parts fails its lstat hears the fault's
// class, as for the book's folder (TestAskedBookRefusedForGoodIsNotIndexable):
// a fault a retry may get past is a look given up on (503), any other the
// lasting 404, never the same retry loop one level down. Not parallel: it
// swaps lstatAudio.
func TestAskedBookPartFaultIsClassified(t *testing.T) {
	root, _, _, asks := askedEnv(t, "Author/Book/01.mp3", "Author/Book/02.mp3")
	part := filepath.Join(root, "Author", "Book", "02.mp3")
	was := lstatAudio
	t.Cleanup(func() { lstatAudio = was })
	for _, c := range faultClasses {
		lstatAudio = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
			if p == part {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: c.errno}
			}
			return was(ctx, p, timeout)
		}
		for name, ask := range asks {
			_, err := ask("Author/Book")
			checkAsked(t, name+" with a part's lstat "+c.errno.Error(), err, c.transient)
		}
	}
	lstatAudio = was
	if _, err := asks["IndexPath"]("Author/Book"); err != nil {
		t.Errorf("IndexPath with every look answering: %v", err)
	}
}

// A folder with one part refused for good (a permission) and another failing
// as a share that may come back (EIO) is a look given up on, whichever of the
// two is looked at first: the transient fault wins, so the book is never
// answered as gone for good on a share's hiccup. Not parallel: it swaps
// lstatAudio.
func TestStatAudioTransientFaultWinsInAFolder(t *testing.T) {
	was := lstatAudio
	t.Cleanup(func() { lstatAudio = was })
	faults := map[string]syscall.Errno{"/lib/Book/01.mp3": syscall.EACCES, "/lib/Book/02.mp3": syscall.EIO}
	lstatAudio = func(_ context.Context, p string, _ time.Duration) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: faults[p]}
	}
	for _, order := range [][]string{{"01.mp3", "02.mp3"}, {"02.mp3", "01.mp3"}} {
		var audio []audioFile
		for _, name := range order {
			audio = append(audio, audioFile{abs: "/lib/Book/" + name})
		}
		// One at a time, so the files are looked at in this order.
		_, unanswered, err := statAudio(t.Context(), map[string][]audioFile{"Book": audio}, 1, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := unanswered["Book"]; !metadata.GaveUp(got) || faultedForGood(got) || !errors.Is(got, syscall.EIO) {
			t.Errorf("parts looked at as %v: the folder's error is %v, want the EIO a retry may get past", order, got)
		}
		if got := askedFault(unanswered); !metadata.GaveUp(got) || errors.As(got, new(lookRefused)) {
			t.Errorf("parts looked at as %v: a caller who asked hears %v, want a look given up on", order, got)
		}
	}
}

// A disc of a joined book (a `book` override over a disc set) whose sibling
// disc folder fails its listing hears the fault's class: the joined book
// can't be read now (transient: a look given up on) or at all (any other: the
// lasting 404). Not parallel: it swaps the looks.
func TestAskedJoinedDiscSiblingFaultIsClassified(t *testing.T) {
	root, lib, s, asks := askedEnv(t, "Author/Book/CD1/01.mp3", "Author/Book/CD2/01.mp3")
	if err := s.cat.SetFolderOverride(t.Context(), lib.ID, "Author/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	cd2 := filepath.Join(root, "Author", "Book", "CD2")
	for _, c := range faultClasses {
		failLooks(t, map[string]syscall.Errno{cd2: c.errno})
		for name, ask := range asks {
			_, err := ask("Author/Book/CD1")
			checkAsked(t, name+" with a sibling disc's listing "+c.errno.Error(), err, c.transient)
		}
	}
}

// A disc folder whose sibling disc refuses the server's user for good (a
// permission) reads on demand as itself, the disc of a set split across its
// folder's discs, never a 503 share_not_answering and never a book of its own
// for want of the sibling's listing. Denied: a sibling that stopped answering
// (EIO) beside the refused one is a look given up on whatever the map's order.
// Not parallel: it swaps the looks.
func TestAskedDiscWithARefusedSibling(t *testing.T) {
	root, lib, s, asks := askedEnv(t, "Author/Book/CD1/01.mp3", "Author/Book/CD2/01.mp3", "Author/Book/CD3/01.mp3")
	cd2, cd3 := filepath.Join(root, "Author", "Book", "CD2"), filepath.Join(root, "Author", "Book", "CD3")
	failLooks(t, map[string]syscall.Errno{cd2: syscall.EACCES, cd3: syscall.EACCES})
	for name, ask := range asks {
		b, err := ask("Author/Book/CD1")
		if err != nil || b == nil || b.RelPath != "Author/Book/CD1" {
			t.Errorf("%s of a disc with its siblings refused: %+v, %v; want the disc", name, b, err)
		}
		if got := splitParents(t, s.cat, lib.ID)["Author/Book/CD1"]; got != "Author/Book" {
			t.Errorf("%s of a disc with its siblings refused: split from %q, want Author/Book", name, got)
		}
	}
	for range 5 { // a map's order is no answer
		failLooks(t, map[string]syscall.Errno{cd2: syscall.EACCES, cd3: syscall.EIO})
		for name, ask := range asks {
			if _, err := ask("Author/Book/CD1"); !metadata.GaveUp(err) {
				t.Errorf("%s of a disc with a sibling refused and one not answering: %v, want a look given up on", name, err)
			}
		}
	}
}

// A disc folder whose set can't be known for good is not indexable on demand,
// rather than a book of its own with no set: the set's own folder refusing
// its listing (traversable, not listable), or a folder in it with no disc's
// name refused, which may hold the audio that makes it no disc set. Not
// parallel: it swaps the looks.
func TestAskedDiscWhoseSetIsRefused(t *testing.T) {
	root, lib, s, asks := askedEnv(t, "Author/Book/CD1/01.mp3", "Author/Book/CD2/01.mp3", "Author/Book/Bonus/01.mp3")
	for _, refused := range []string{filepath.Join(root, "Author", "Book"), filepath.Join(root, "Author", "Book", "Bonus")} {
		failLooks(t, map[string]syscall.Errno{refused: syscall.EACCES})
		for name, ask := range asks {
			_, err := ask("Author/Book/CD1")
			checkAsked(t, name+" of a disc with "+refused+" refused", err, false)
		}
		if b, err := s.cat.GetBookByPath(t.Context(), lib.ID, "Author/Book/CD1"); !errors.Is(err, catalog.ErrNotFound) {
			t.Errorf("with %s refused the disc was indexed: %+v, %v; want no book", refused, b, err)
		}
	}
}

// A request's looks while its book is resolved end together at its wait cap
// (WithWaitCap), not only each at its look time: a share that answers each
// look just inside its time never holds the request past the cap, its answer a
// look given up on. Allowed: the same looks under a cap they fit in resolve
// the book. Not parallel: it swaps the looks.
func TestResolveAskedEndsAtTheRequestsWaitCap(t *testing.T) {
	_, lib, s, _ := askedEnv(t, "Author/Book/01.mp3")
	wasDir, wasStat := readDirLook, statLook
	t.Cleanup(func() { readDirLook, statLook = wasDir, wasStat })
	const slow = 200 * time.Millisecond
	readDirLook = func(ctx context.Context, p string, timeout time.Duration) ([]fs.DirEntry, error) {
		time.Sleep(slow)
		return wasDir(ctx, p, timeout)
	}
	statLook = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
		time.Sleep(slow)
		return wasStat(ctx, p, timeout)
	}
	asked := func(capAt time.Duration) context.Context {
		return WithWaitCap(WithLookTimeout(t.Context(), 5*time.Second), time.Now().Add(capAt))
	}
	began := time.Now()
	_, err := s.resolveAsked(asked(300*time.Millisecond), *lib, "Author/Book", nil)
	if took := time.Since(began); !metadata.GaveUp(err) || took > 2*time.Second {
		t.Errorf("the looks past the cap: %v after %v, want a look given up on at the cap", err, took)
	}
	if b, err := s.resolveAsked(asked(30*time.Second), *lib, "Author/Book", nil); err != nil || b.RelPath != "Author/Book" {
		t.Errorf("the looks within the cap: %+v, %v; want the book", b, err)
	}
}

// A request that leads the read of its book (its own Re-read, or the first
// ask for a book not indexed yet) waits for it as long as a request joining
// it may (waitCapLeft, at its WithWaitCap), then gives up as a look not
// answered (errReadUnfinished: 503 share_not_answering with Retry-After)
// rather than wait out the request's own time, the read left to go on. A
// caller with no cap (a scan, a library caller) waits until its context ends.
func TestAwaitFlightCapsTheLeadsWait(t *testing.T) {
	t.Parallel()
	s := &Scanner{probeTimeout: 3 * time.Second}
	f := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	// began is read first: the cap is 300ms from a moment no earlier than it.
	began := time.Now()
	capped := WithWaitCap(WithLookTimeout(t.Context(), 100*time.Millisecond), time.Now().Add(300*time.Millisecond))
	_, err := s.awaitFlight(capped, f, false)
	if took := time.Since(began); !errors.Is(err, errReadUnfinished) || !metadata.GaveUp(err) || took < 300*time.Millisecond || took >= s.probeTimeout {
		t.Errorf("the lead's wait capped at 300ms: %v after %v, want errReadUnfinished at the cap", err, took)
	}
	if f.stuck.Load() {
		t.Error("the lead's own read was marked stuck")
	}
	// A look time but no cap (a library caller): bounded by its context alone.
	ctx, cancel := context.WithTimeout(WithLookTimeout(t.Context(), 100*time.Millisecond), 400*time.Millisecond)
	defer cancel()
	if _, err := s.awaitFlight(ctx, f, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an uncapped lead's wait: %v, want its context's end", err)
	}
}
