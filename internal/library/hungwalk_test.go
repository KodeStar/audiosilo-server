package library

import (
	"log/slog"
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
