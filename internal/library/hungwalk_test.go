package library

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// A move's old path on a share that stopped answering (IndexMoved's
// bookOnDisk) is given up on at the look's time and counts as still on disk:
// the move isn't carried, and the new path is indexed as a book of its own.
// Not parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestIndexMovedWithTheOldPathHeldGivesUp(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Old/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = time.Minute
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
		t.Fatalf("added %d, want the book", res.Added)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}

	// Old is gone, but on a share that stopped answering that can't be known.
	release := metadata.HoldOpensForTests(filepath.Join(root, "Old"))
	t.Cleanup(release)
	start := time.Now()
	b, err := s.IndexMoved(WithLookTimeout(ctx, 100*time.Millisecond), *lib, "New/Book", "Old/Book")
	if err != nil || b.RelPath != "New/Book" {
		t.Fatalf("IndexMoved with the old path held = %+v, %v; want the new book", b, err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("IndexMoved took %v, want the old path given up on at the look's time", took)
	}
	if _, err := cat.GetBookByPath(ctx, lib.ID, "Old/Book"); err != nil {
		t.Errorf("the old book, not known gone, = %v; want it kept", err)
	}

	release()
	if b, err := s.IndexMoved(WithLookTimeout(ctx, 10*time.Second), *lib, "New/Book", "Old/Book"); err != nil || b.RelPath != "New/Book" {
		t.Fatalf("IndexMoved once the share answers = %+v, %v", b, err)
	}
	if _, err := cat.GetBookByPath(ctx, lib.ID, "Old/Book"); err == nil {
		t.Error("the old book once known gone was kept, want the move carried")
	}
}
