package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A book's cover_version is its art file's own version from the scan on
// (ADMIN-01, UPG-02): a re-index of unchanged art keeps the version and the
// colour read for it; a sidecar replaced in place gets a version it never had
// before, from a scan that finds the audio unchanged and from a re-index alike.
func TestCoverVersionFollowsArtFile(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	cover := filepath.Join(root, "Author", "Book", "cover.jpg")
	writeFile(t, root, "Author/Book/cover.jpg", []byte("first art"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "C", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	state := func() (*catalog.CoverColor, string) {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		return b.CoverColor, b.CoverVersion
	}
	fileVersion := func() string {
		t.Helper()
		fi, err := os.Stat(cover)
		if err != nil {
			t.Fatal(err)
		}
		return catalog.FileArtVersion(catalog.ArtSidecar, fi.Size(), fi.ModTime())
	}
	_, v0 := state()
	if want := catalog.CoverVersion(fileVersion()); v0 != want {
		t.Fatalf("first scan: version %q, want the art file's %q", v0, want)
	}
	// A thumbnail read records its colour under the same identity.
	src, err := cat.CoverSources(ctx, lib.ID, []string{"Author/Book"})
	if err != nil {
		t.Fatal(err)
	}
	color := catalog.CoverColor{Bg: "#336699"}
	if err := cat.RecordCoverColors(ctx, []catalog.CoverColorRecord{{LibraryID: lib.ID, Path: "Author/Book",
		Art: src["Author/Book"].Art, Version: fileVersion(), Color: color}}); err != nil {
		t.Fatal(err)
	}
	if cc, v := state(); cc == nil || *cc != color || v != v0 {
		t.Fatalf("after a read: %+v %q, want %+v %q", cc, v, color, v0)
	}

	// Re-index the unchanged art: version and colour stay.
	if _, err := s.IndexPath(ctx, *lib, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	if cc, v := state(); cc == nil || *cc != color || v != v0 {
		t.Fatalf("re-index of unchanged art: %+v %q, want %+v %q", cc, v, color, v0)
	}

	// Replace the art in place (same name, other bytes and mtime): a scan that
	// finds the audio unchanged moves the version.
	seen := map[string]bool{v0: true}
	replace := func(data string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(cover, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(cover, at, at); err != nil {
			t.Fatal(err)
		}
	}
	replace("second, other art", time.Now().Add(time.Hour))
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	cc, v1 := state()
	if cc != nil || seen[v1] || v1 != catalog.CoverVersion(fileVersion()) {
		t.Fatalf("after replace + scan: %+v %q, want no colour and a new version", cc, v1)
	}
	seen[v1] = true

	// Replace again, and this time re-index the book: still a new version, never
	// back to an earlier one.
	replace("third", time.Now().Add(2*time.Hour))
	if _, err := s.IndexPath(ctx, *lib, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	if _, v2 := state(); seen[v2] || v2 != catalog.CoverVersion(fileVersion()) {
		t.Fatalf("after replace + re-index: version %q, want a new one (seen %v)", v2, seen)
	}
}
