package library

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// One hung share stops reads only from itself (server#360): with the reads on
// library A's share stuck (its mount's count of hung reads at the cap), a
// re-index of A refuses the read and keeps its book whole, while library B, on
// another mount, reads its changed book as ever. Once A's share answers, A
// reads again. Not parallel: metadata.HangMountForTests swaps the mount lookup.
func TestHungShareLeavesOtherLibrariesReading(t *testing.T) {
	ffprobe, _ := exec.LookPath("ffprobe") // "" reads in process: either way
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	s := NewScanner(cat, ffprobe, slog.Default())
	type side struct {
		root string
		lib  *catalog.Library
	}
	sides := map[string]*side{}
	for _, name := range []string{"A", "B"} {
		root := t.TempDir()
		silence(t, root, "Author/Book/01.mp3", 1, "album=Title "+name, "artist=Real Author")
		lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: name, Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
		sides[name] = &side{root, lib}
	}
	read := func(name string) (title, scanError, detail string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT title, scan_error, scan_error_detail FROM books
			WHERE library_id = ? AND rel_path = 'Author/Book'`, sides[name].lib.ID).Scan(&title, &scanError, &detail); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Both books change on disk (retagged), so both are read again.
	for name, sd := range sides {
		silence(t, sd.root, "Author/Book/01.mp3", 1, "album=New "+name, "artist=Real Author")
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(sd.root, "Author", "Book", "01.mp3"), later, later); err != nil {
			t.Fatal(err)
		}
	}

	release := metadata.HangMountForTests(sides["A"].root)
	t.Cleanup(release)
	resA, err := s.Scan(ctx, *sides["A"].lib)
	if err != nil {
		t.Fatal(err)
	}
	resB, err := s.Scan(ctx, *sides["B"].lib)
	if err != nil {
		t.Fatal(err)
	}
	if title, problem, detail := read("A"); title != "Title A" || problem != problemUnreadable || detail != readRefusedDetail || resA.Updated != 0 {
		t.Errorf("A on the hung share = %q, %q (%q), %d updated; want kept whole, its read refused", title, problem, detail, resA.Updated)
	}
	if title, problem, _ := read("B"); title != "New B" || problem != "" || resB.Updated != 1 {
		t.Errorf("B beside the hung share = %q, %q, %d updated; want it read", title, problem, resB.Updated)
	}

	release()
	if _, err := s.Scan(ctx, *sides["A"].lib); err != nil {
		t.Fatal(err)
	}
	if title, problem, _ := read("A"); title != "New A" || problem != "" {
		t.Errorf("A once its share answers = %q, %q; want it read", title, problem)
	}
}
