package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan whose walk reaches a folder on a share that stopped answering
// (SRV-15c: the walk's own listings and lstats were unbounded, and one run sat
// at 4/10 for 3m19s) gives up on it at the probe's time and goes on: a folder
// that won't list, or one with an audio file whose lstat doesn't answer, is
// skipped for this run (partial, so its book is kept as stored, never pruned
// nor rewritten without a part) and recorded as an unreadable path; a
// .audiosilo folder that won't list keeps its book's attachments as stored.
// The rest of the library scans as ever. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestAScanSkipsAFolderThatDoesNotAnswer(t *testing.T) {
	for _, c := range []struct {
		name, held, logged string
		partial            bool
	}{
		{"listing", "Author/Stuck", "Author/Stuck", true},
		{"audio lstat", "Author/Stuck/01.mp3", "Author/Stuck", true},
		{"attachments", "Author/Stuck/.audiosilo", "Author/Stuck/.audiosilo", false},
		{"metadata.json", "Author/Stuck/metadata.json", "Author/Stuck/metadata.json", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, root, "Author/Stuck/01.mp3", make([]byte, 1<<10))
			writeFile(t, root, "Author/Stuck/metadata.json", []byte(`{"title":"ABS Title"}`))
			writeFile(t, root, "Author/Stuck/.audiosilo/characters.json", []byte(`{}`))
			writeFile(t, root, "Author/Fine/01.mp3", make([]byte, 1<<10))
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = 200 * time.Millisecond
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 2 {
				t.Fatalf("added %d, want both books", res.Added)
			}
			writeFile(t, root, "Author/Fine/02.mp3", make([]byte, 1<<10)) // a change the scan picks up

			release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(c.held)))
			t.Cleanup(release)
			res := scanWithin(t, s, *lib, 20*time.Second)
			if res.Partial != c.partial || res.Removed != 0 || res.Updated != 1 {
				t.Errorf("scan with %s held: partial %v, removed %d, updated %d; want partial %v, none removed, the other book updated",
					c.held, res.Partial, res.Removed, res.Updated, c.partial)
			}
			logged := slices.ContainsFunc(res.Log, func(e catalog.RunEvent) bool { return e.Kind == "unreadable" && e.Path == c.logged })
			if !logged {
				t.Errorf("scan with %s held logged %+v, want %q unreadable", c.held, res.Log, c.logged)
			}
			if b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Stuck"); err != nil || len(b.Files) != 1 {
				t.Errorf("the book on the held share = %v (%v), want it kept as stored", b, err)
			}

			release()
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Partial || res.Removed != 0 {
				t.Errorf("the scan once the share answers: partial %v, removed %d; want a whole walk", res.Partial, res.Removed)
			}
		})
	}
}

// An audio file's lstat that fails as a share does (EIO, ESTALE: a
// soft-mounted NFS share; ENOTCONN: a FUSE pool; EACCES) says nothing about
// whether the file is there: its folder is skipped for the run as one whose look was given up on
// (partial, an unreadable path logged), so a folder book keeps its parts and a
// single-file book its row. ENOENT is the file gone: the part drops. Not
// parallel: it swaps lstatAudio.
func TestAScanKeepsABookWhoseAudioLstatFails(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		parts int // the folder book's after the scan: 1 once its part is gone
	}{
		{"EIO", syscall.EIO, 2},
		{"ESTALE", syscall.ESTALE, 2},
		{"EACCES", syscall.EACCES, 2},
		{"EPERM", syscall.EPERM, 2},
		{"ENOTCONN", syscall.ENOTCONN, 2}, // a FUSE pool that dropped
		{"ENOENT", syscall.ENOENT, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
			writeFile(t, root, "Author/Book/02.mp3", make([]byte, 1<<10))
			writeFile(t, root, "Loose.mp3", make([]byte, 1<<10))
			writeFile(t, root, "Author/Fine/01.mp3", make([]byte, 1<<10)) // the rest of the library
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 3 {
				t.Fatalf("added %d, want the three books", res.Added)
			}

			failing := map[string]bool{
				filepath.Join(root, "Author", "Book", "02.mp3"): true,
				filepath.Join(root, "Loose.mp3"):                true,
			}
			was := lstatAudio
			lstatAudio = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
				if failing[p] {
					return nil, &fs.PathError{Op: "lstat", Path: p, Err: c.err}
				}
				return was(ctx, p, timeout)
			}
			t.Cleanup(func() { lstatAudio = was })
			res := scanWithin(t, s, *lib, 20*time.Second)
			if !res.Partial || res.Removed != 0 {
				t.Errorf("scan with %v: partial %v, removed %d; want a partial run, none removed", c.err, res.Partial, res.Removed)
			}
			wantLogged := c.parts == 2 // a share's fault, not the files gone
			for _, dir := range []string{"Author/Book", ""} {
				logged := slices.ContainsFunc(res.Log, func(e catalog.RunEvent) bool { return e.Kind == "unreadable" && e.Path == dir })
				if logged != wantLogged {
					t.Errorf("scan with %v logged %+v; want %q unreadable: %v", c.err, res.Log, dir, wantLogged)
				}
			}
			if b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Book"); err != nil || len(b.Files) != c.parts {
				t.Errorf("the folder book after %v = %+v (%v), want %d parts", c.err, b, err, c.parts)
			}
			if _, err := cat.GetBookByPath(ctx, lib.ID, "Loose.mp3"); err != nil {
				t.Errorf("the single-file book after %v = %v, want its row kept", c.err, err)
			}
		})
	}
}

// bookOnDisk tells a book it can't look at now (an audio file's lstat failing
// as a share does that a retry may get past) from one it can't check for good
// (a permission: present, its state kept where it is) and from one gone. Not
// parallel: it swaps lstatAudio.
func TestBookOnDiskTellsAShareFaultFromAPermission(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	part := filepath.Join(root, "Author", "Book", "01.mp3")
	was := lstatAudio
	t.Cleanup(func() { lstatAudio = was })
	for _, c := range []struct {
		err  error
		want onDisk
	}{
		{nil, diskPresent},
		{syscall.EIO, diskUnknown},
		{syscall.ENOTCONN, diskUnknown},
		{syscall.EACCES, diskPresent},
		{syscall.ELOOP, diskPresent},
	} {
		lstatAudio = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
			if c.err != nil && p == part {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: c.err}
			}
			return was(ctx, p, timeout)
		}
		if got := s.bookOnDisk(ctx, *lib, "Author/Book"); got != c.want {
			t.Errorf("bookOnDisk with its audio's lstat failing %v = %v, want %v", c.err, got, c.want)
		}
	}
	lstatAudio = was
	if got := s.bookOnDisk(ctx, *lib, "Author/Gone"); got != diskGone {
		t.Errorf("bookOnDisk of a path not on disk = %v, want gone", got)
	}
}

// A fingerprint missing for a reason a retry may get past (given up on, a
// share's EIO or ENOTCONN, a read cut short) leaves a move unknown; one
// refused for good (a permission, the device gone, no regular file, the file
// gone) does not.
func TestFingerprintUnknown(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{metadata.ErrNotAnswered, true},
		{metadata.ErrReadRefused, true},
		{&fs.PathError{Op: "read", Err: syscall.EIO}, true},
		{&fs.PathError{Op: "open", Err: syscall.ENOTCONN}, true},
		{io.ErrUnexpectedEOF, true},
		{&fs.PathError{Op: "open", Err: syscall.EACCES}, false},
		{&fs.PathError{Op: "open", Err: syscall.ENXIO}, false},
		{metadata.ErrNotRegular, false},
		{&fs.PathError{Op: "open", Err: syscall.ENOENT}, false},
		{nil, false},
	} {
		if got := fingerprintUnknown(c.err); got != c.want {
			t.Errorf("fingerprintUnknown(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// The scan's other reads on a share that stopped answering are bounded too
// (SRV-15c): a folder book's tag-only suspect check (suspectFromTags) and a
// read-along ebook's look and hash (EbookDigestNow) are given up on at their
// time. Not parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestScanTagAndHashReadsOnAHeldShareGiveUp(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Book/01.mp3", make([]byte, 1<<10))
	writeFile(t, root, "Book/Book.epub", epubBytes(t, minimalEpub...))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	release := metadata.HoldOpensForTests(filepath.Join(root, "Book"))
	t.Cleanup(release)

	start := time.Now()
	b := &catalog.Book{RelPath: "Book", IsFolder: true, Files: []catalog.BookFile{{RelPath: "Book/01.mp3"}}}
	if _, ok := suspectFromTags(ctx, *lib, b, 100*time.Millisecond); ok {
		t.Error("the suspect check on a held share read the tags, want it left for the next scan")
	}
	if _, err := s.EbookDigestNow(ctx, *lib, catalog.ReadalongIndexed{}, "Book", "Book/Book.epub", true, 100*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("the ebook's digest on a held share = %v, want a look given up on", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the reads took %v, want each given up on at its time", took)
	}

	release()
	if _, ok := suspectFromTags(ctx, *lib, b, time.Minute); !ok {
		t.Error("the suspect check once the share answers did not read the tags")
	}
	if sha, err := s.EbookDigestNow(ctx, *lib, catalog.ReadalongIndexed{}, "Book", "Book/Book.epub", true, time.Minute); err != nil || sha == "" {
		t.Errorf("the ebook's digest once the share answers = %q, %v; want it hashed", sha, err)
	}
}

// A disc folder of a joined book (a `book` override on a disc set) that doesn't
// answer (SRV-15c: its listing, or a file's lstat, given up on) leaves the
// joined book as stored: a scan drops the other discs too (dropUnanswered), so
// it never rewrites the book without that disc's parts, nor, with two discs, as
// a lone disc's book once the set reads as none; a Re-read of it is a look given
// up on. Not parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestAJoinedBookWithADiscThatDoesNotAnswerIsKept(t *testing.T) {
	for _, discs := range []int{2, 3} {
		for _, held := range []string{"Book/CD2", "Book/CD2/01.mp3"} {
			t.Run(fmt.Sprintf("%d discs, %s", discs, held), func(t *testing.T) {
				ctx := t.Context()
				db := storetest.Open(t)
				cat := catalog.New(db, time.Now)
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				for d := 1; d <= discs; d++ {
					writeFile(t, root, fmt.Sprintf("Book/CD%d/01.mp3", d), make([]byte, 1<<10))
				}
				writeFile(t, root, "Other/01.mp3", make([]byte, 1<<10)) // the library holds more than the held book
				lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
				if err := cat.SetFolderOverride(ctx, lib.ID, "Book", catalog.OverrideBook); err != nil {
					t.Fatal(err)
				}
				s := NewScanner(cat, "", slog.Default())
				s.probeTimeout = 200 * time.Millisecond
				if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 2 {
					t.Fatalf("added %d, want the joined book and the other", res.Added)
				}
				writeFile(t, root, "Book/CD1/02.mp3", make([]byte, 1<<10)) // a change that would re-read it

				release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(held)))
				t.Cleanup(release)
				res := scanWithin(t, s, *lib, 20*time.Second)
				if !res.Partial || res.Added != 0 || res.Updated != 0 || res.Removed != 0 {
					t.Errorf("scan with %s held: partial %v, added %d, updated %d, removed %d; want partial, the joined book kept",
						held, res.Partial, res.Added, res.Updated, res.Removed)
				}
				if b, err := cat.GetBookByPath(ctx, lib.ID, "Book"); err != nil || len(b.Files) != discs {
					t.Errorf("the joined book = %v (%v), want it kept with its %d parts", b, err, discs)
				}
				if _, err := cat.GetBookByPath(ctx, lib.ID, "Book/CD1"); err == nil {
					t.Error("the first disc was indexed as a book of its own")
				}
				if _, err := s.ReindexPath(WithLookTimeout(ctx, 100*time.Millisecond), *lib, "Book/CD1"); !metadata.GaveUp(err) {
					t.Errorf("Re-read of a disc with %s held = %v, want a look given up on", held, err)
				}

				release()
				if b, err := s.ReindexPath(WithLookTimeout(ctx, 10*time.Second), *lib, "Book/CD1"); err != nil || b.RelPath != "Book" || len(b.Files) != discs+1 {
					t.Errorf("Re-read once the share answers = %+v, %v; want the joined book with the new part", b, err)
				}
			})
		}
	}
}

// The Bridge's attachments rescan (RefreshAttachments) on a share that stopped
// answering gives up at the on-demand look's time (WithLookTimeout), its error
// the look given up on (metadata.GaveUp: the API's 503 share_not_answering),
// whether the join, the listing or an attachment's read is what hangs; the
// stored attachments stay. Not parallel: metadata.HoldOpensForTests swaps the
// mount lookup.
func TestRefreshAttachmentsOnAHeldShareGivesUp(t *testing.T) {
	for _, c := range []struct {
		name, held string
		reads      bool // only the reads hang (HoldReadsForTests)
	}{
		{"the folder", "A/Book", false},
		{"an attachment's read", "A/Book/.audiosilo/characters.json", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, root, "A/Book/01.mp3", make([]byte, 1<<10))
			writeFile(t, root, "A/Book/.audiosilo/characters.json", []byte(testCharacters))
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = time.Minute // the look's time, not the probe's, ends it
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
				t.Fatalf("added %d, want the book", res.Added)
			}
			writeFile(t, root, "A/Book/.audiosilo/recaps.json", []byte(testRecaps)) // a change to read

			hold := metadata.HoldOpensForTests
			if c.reads {
				hold = metadata.HoldReadsForTests
			}
			release := hold(filepath.Join(root, filepath.FromSlash(c.held)))
			t.Cleanup(release)
			start := time.Now()
			if _, err := s.RefreshAttachments(WithLookTimeout(ctx, 100*time.Millisecond), *lib, "A/Book"); !metadata.GaveUp(err) {
				t.Errorf("refresh with %s held = %v, want a look given up on", c.held, err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("refresh took %v, want it given up on at the look's time", took)
			}
			if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a == nil || a.Characters == nil || a.Recaps != nil {
				t.Errorf("stored after the refresh gave up = %+v, want the first scan's", a)
			}

			release()
			if got, err := s.RefreshAttachments(WithLookTimeout(ctx, 10*time.Second), *lib, "A/Book"); err != nil || !got.Attachments.Recaps {
				t.Errorf("refresh once the share answers = %+v, %v; want the recaps read", got, err)
			}
		})
	}
}

// An on-demand index of a moved book that can't tell now whether the book
// moved (its old path on a share that stopped answering, given up on at the
// look's time, or the new book's fingerprint unread for its audio's reads held
// on a share whose looks still answer) carries and indexes nothing, a look
// given up on (the API's and the Bridge's 503 share_not_answering), so the old
// book keeps its row and its state rather than a new book indexed beside a row
// whose state the next scan's prune would strand. IndexMoved is told the old
// path; IndexPath and ReindexPath find it themselves (movedFrom). Once the
// share answers, the same call carries the state. Not parallel:
// metadata.HoldOpensForTests and HoldReadsForTests swap the mount lookup.
func TestAnIndexWithAMoveUnknownGivesUp(t *testing.T) {
	for _, c := range []struct {
		name  string
		index indexCall
	}{
		{"IndexMoved", (*movedBook).indexMoved},
		{"IndexPath", (*movedBook).indexPath},
		{"ReindexPath", (*movedBook).reindexPath},
	} {
		t.Run(c.name+"/old path held", func(t *testing.T) {
			m := newMovedBook(t)
			m.s.probeTimeout = time.Minute
			release := metadata.HoldOpensForTests(filepath.Join(m.root, "Old"))
			t.Cleanup(release)
			start := time.Now()
			m.wantUnknown(t, WithLookTimeout(m.ctx, 100*time.Millisecond), c.index)
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("%s took %v, want the old path given up on at the look's time", c.name, took)
			}
			release()
			m.wantCarried(t, c.index)
		})
		t.Run(c.name+"/new book unread", func(t *testing.T) {
			m := newMovedBook(t)
			m.s.probeTimeout = 200 * time.Millisecond // the fingerprint's time
			release := metadata.HoldReadsForTests(filepath.Join(m.root, "New", "Book", "01.mp3"))
			t.Cleanup(release)
			m.wantUnknown(t, WithLookTimeout(m.ctx, 10*time.Second), c.index)
			release()
			m.s.probeTimeout = time.Minute
			m.wantCarried(t, c.index)
		})
	}
}

// movedBook is a library whose one book, scanned at Old/Book with a
// listener's progress, has since been renamed to New/Book on disk, for
// IndexMoved's tests.
type movedBook struct {
	ctx  context.Context
	cat  *catalog.Catalog
	s    *Scanner
	lib  *catalog.Library
	root string
	uid  int64
}

func newMovedBook(t *testing.T) *movedBook {
	t.Helper()
	m := &movedBook{ctx: t.Context()}
	db := storetest.Open(t)
	m.cat = catalog.New(db, time.Now)
	var err error
	if m.root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, m.root, "Old/Book/01.mp3", make([]byte, 1<<10))
	m.lib, _ = m.cat.CreateLibrary(m.ctx, catalog.Library{Name: "L", Root: m.root})
	m.s = NewScanner(m.cat, "", slog.Default())
	if res := scanWithin(t, m.s, *m.lib, 20*time.Second); res.Added != 1 {
		t.Fatalf("added %d, want the book", res.Added)
	}
	m.uid = seedUserID(t, db)
	if _, err := m.cat.SaveProgress(m.ctx, m.uid, catalog.Progress{Ref: m.ref("Old/Book"), Position: 42}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(m.root, "Old"), filepath.Join(m.root, "New")); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *movedBook) ref(p string) catalog.Ref { return catalog.Ref{LibraryID: m.lib.ID, Path: p} }

// indexCall is an on-demand index of the moved book's new path, New/Book.
type indexCall func(m *movedBook, ctx context.Context) (*catalog.Book, error)

// indexMoved is the Bridge's IndexMoved from Old/Book; indexPath and
// reindexPath index New/Book with no from, finding the move themselves
// (movedFrom).
func (m *movedBook) indexMoved(ctx context.Context) (*catalog.Book, error) {
	return m.s.IndexMoved(ctx, *m.lib, "New/Book", "Old/Book")
}

func (m *movedBook) indexPath(ctx context.Context) (*catalog.Book, error) {
	return m.s.IndexPath(ctx, *m.lib, "New/Book")
}

func (m *movedBook) reindexPath(ctx context.Context) (*catalog.Book, error) {
	return m.s.ReindexPath(ctx, *m.lib, "New/Book")
}

// wantUnknown checks index under ctx is a look given up on that carries and
// indexes nothing: the old book keeps its row and progress, and the new path
// stays new for the retry or a scan.
func (m *movedBook) wantUnknown(t *testing.T, ctx context.Context, index indexCall) {
	t.Helper()
	if b, err := index(m, ctx); !metadata.GaveUp(err) {
		t.Fatalf("an index with the move unknown = %+v, %v; want a look given up on", b, err)
	}
	if _, err := m.cat.GetBookByPath(m.ctx, m.lib.ID, "Old/Book"); err != nil {
		t.Errorf("the old book, not known moved, = %v; want it kept", err)
	}
	if b, err := m.cat.GetBookByPath(m.ctx, m.lib.ID, "New/Book"); err == nil {
		t.Errorf("the new book was indexed (%+v) with its move unknown, want it left new", b)
	}
	if got, _ := m.cat.GetProgress(m.ctx, m.uid, m.ref("New/Book")); got != nil {
		t.Errorf("progress at the new path = %+v, want nothing carried", got)
	}
}

// wantCarried checks index, once the share answers, carries the move.
func (m *movedBook) wantCarried(t *testing.T, index indexCall) {
	t.Helper()
	if b, err := index(m, WithLookTimeout(m.ctx, 10*time.Second)); err != nil || b.RelPath != "New/Book" {
		t.Fatalf("an index once the share answers = %+v, %v", b, err)
	}
	if _, err := m.cat.GetBookByPath(m.ctx, m.lib.ID, "Old/Book"); err == nil {
		t.Error("the old book once known moved was kept, want the move carried")
	}
	if got, _ := m.cat.GetProgress(m.ctx, m.uid, m.ref("New/Book")); got == nil || got.Position != 42 {
		t.Errorf("progress at the new path once the share answers = %+v, want it carried", got)
	}
}

// A request that joins IndexPath's read of a book whose move can't be told
// gets that read's answer, the same look given up on: neither a book indexed
// without its state nor a wait past the read. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestIndexPathJoinedWithAMoveUnknownGivesUp(t *testing.T) {
	m := newMovedBook(t)
	m.s.probeTimeout = time.Minute
	release := metadata.HoldOpensForTests(filepath.Join(m.root, "Old"))
	t.Cleanup(release)

	lead := make(chan error, 1)
	go func() {
		_, err := m.indexPath(WithLookTimeout(m.ctx, time.Second))
		lead <- err
	}()
	waitFlight(t, m.s, *m.lib, "New/Book", true)
	// The joined request's own look time outlasts the leader's, so the answer
	// it gets is the leader's read's, not its own wait given up on.
	b, err := m.indexPath(WithLookTimeout(m.ctx, 30*time.Second))
	if !metadata.GaveUp(err) || errors.Is(err, errReadLeft) {
		t.Errorf("the joined request = %+v, %v; want the read's look given up on", b, err)
	}
	if err := <-lead; !metadata.GaveUp(err) {
		t.Errorf("the leading request = %v, want a look given up on", err)
	}
	m.wantUnknown(t, WithLookTimeout(m.ctx, 100*time.Millisecond), (*movedBook).indexPath)

	release()
	m.wantCarried(t, (*movedBook).indexPath)
}

// A book in a folder the walk gave up on is not known gone (SRV-15c): a copy
// of it the same partial run finds elsewhere is a new book, never the stuck
// book moved, so its listening state stays on its path; a book that did move
// in that run (its old path gone from disk) still carries its state. Not
// parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestAPartialScanMovesNoStateFromAFolderThatDoesNotAnswer(t *testing.T) {
	for _, held := range []string{"Author/Stuck", "Author/Stuck/01.mp3"} {
		t.Run(held, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			stuck, moving := make([]byte, 1<<10), make([]byte, 2<<10)
			writeFile(t, root, "Author/Stuck/01.mp3", stuck)
			writeFile(t, root, "Author/Moving/01.mp3", moving)
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = 200 * time.Millisecond
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 2 {
				t.Fatalf("added %d, want both books", res.Added)
			}
			uid := seedUserID(t, db)
			ref := func(p string) catalog.Ref { return catalog.Ref{LibraryID: lib.ID, Path: p} }
			for _, p := range []string{"Author/Stuck", "Author/Moving"} {
				if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: ref(p), Position: 42}); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, root, "Other/Copy/01.mp3", stuck) // the stuck book's bytes, and so its fingerprint
			if err := os.Rename(filepath.Join(root, "Author/Moving"), filepath.Join(root, "Other/Moved")); err != nil {
				t.Fatal(err)
			}

			release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(held)))
			t.Cleanup(release)
			res := scanWithin(t, s, *lib, 20*time.Second)
			if !res.Partial || res.Moved != 1 || res.Added != 1 {
				t.Errorf("scan with %s held: partial %v, moved %d, added %d; want a partial run, the real move, the copy added",
					held, res.Partial, res.Moved, res.Added)
			}
			for p, want := range map[string]bool{"Author/Stuck": true, "Other/Copy": false, "Author/Moving": false, "Other/Moved": true} {
				if got, _ := cat.GetProgress(ctx, uid, ref(p)); (got != nil) != want {
					t.Errorf("progress at %s = %+v, want it there: %v", p, got, want)
				}
			}
			release()
		})
	}
}

// dropUnanswered drops a folder a look was given up on in, and the disc
// folders of it and of every folder above it: whether those make a disc set
// turns on all the audio in and beneath the folder, the folder not looked at
// included. Without the folder's own audio (Own) or the audio deeper down
// (Book/Extras/Deep), the discs left would read as a set that is none.
func TestDropUnansweredDropsTheDiscsAboveIt(t *testing.T) {
	t.Parallel()
	all := []string{"Book/CD1", "Book/CD2", "Book/Extras/Deep", "Else/CD1", "Else/CD2", "Own", "Own/CD1", "Own/CD2"}
	for _, c := range []struct {
		name, unanswered string
		dropped          []string
	}{
		{"a disc", "Book/CD1", []string{"Book/CD1", "Book/CD2"}},
		{"a folder beside the discs", "Book/Extras", []string{"Book/CD1", "Book/CD2"}},
		{"a folder deeper down", "Book/Extras/Deep", []string{"Book/CD1", "Book/CD2", "Book/Extras/Deep"}},
		{"another book", "Else/CD2", []string{"Else/CD1", "Else/CD2"}},
		{"the folder holding the discs, with audio of its own", "Own", []string{"Own", "Own/CD1", "Own/CD2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dirs := map[string][]audioFile{}
			for _, d := range all {
				dirs[d] = []audioFile{{}}
			}
			dropUnanswered(dirs, map[string]error{c.unanswered: metadata.ErrNotAnswered})
			want := slices.DeleteFunc(slices.Clone(all), func(d string) bool { return slices.Contains(c.dropped, d) })
			if got := slices.Sorted(maps.Keys(dirs)); !slices.Equal(got, want) {
				t.Errorf("left %v, want %v", got, want)
			}
			if sets := discSets(maps.Keys(dirs)); sets["Own"] || sets["Book"] {
				t.Errorf("disc sets left = %v, want neither Own (audio of its own) nor Book (audio deeper down)", sets)
			}
		})
	}
}
