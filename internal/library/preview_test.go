package library

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// previewFixture is a books folder whose tags and folders disagree: a tagged
// book filed under another author's series (tags say Unsouled by Will Wight, the
// folders Other Title, #3 of Some Series by Somebody Else), a tagged loose file at
// the root, and an untagged two-part book whose metadata.json names its author.
func previewFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Somebody Else", "Some Series", "03 - Other Title", "01.m4b"))
	copyChapteredM4B(t, filepath.Join(root, "Loose.m4b"))
	writeFile(t, root, "Author X/Book Y/part1.mp3", []byte("not really audio"))
	writeFile(t, root, "Author X/Book Y/part2.mp3", []byte("not really audio"))
	writeFile(t, root, "Author X/Book Y/metadata.json", []byte(`{"authors":["Abs Author"]}`))
	writeFile(t, root, "Author X/Book Y/cover.txt", []byte("never read"))
	return root
}

func previewScanner() *Scanner { return NewScanner(nil, "", slog.Default()) }

// The preview counts what a scan would find and names every book both ways
// with the catalog's resolver: from the tags (over the path's baseline), and
// from the folders (over the tags). An Audiobookshelf metadata.json goes over
// both. Nothing is indexed (the scanner has no catalog at all).
func TestPreviewNamesBothWays(t *testing.T) {
	t.Parallel()
	root := previewFixture(t)
	p, err := previewScanner().Preview(t.Context(), root, nil, PreviewLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (PreviewCounts{AudioFiles: 4, FolderBooks: 2, FileBooks: 1}); p.Counts != want {
		t.Errorf("counts = %+v, want %+v", p.Counts, want)
	}
	got := map[string]string{}
	for _, s := range p.Samples {
		got[s.RelPath] = fmt.Sprintf("tags=%+v path=%+v abs=%v", s.Tags, s.Path, s.ABS)
	}
	want := map[string]string{
		"Author X/Book Y": "tags={Title:Book Y Author:Abs Author Narrator: Series: Position:} " +
			"path={Title:Book Y Author:Abs Author Narrator: Series: Position:} abs=true",
		"Loose.m4b": "tags={Title:Shade's First Rule Author:A. F. Kay Narrator: Series: Position:} " +
			"path={Title:Loose Author:A. F. Kay Narrator: Series: Position:} abs=false",
		"Somebody Else/Some Series/03 - Other Title": "tags={Title:Unsouled Author:Will Wight Narrator: Series:Some Series Position:3} " +
			"path={Title:Other Title Author:Somebody Else Narrator: Series:Some Series Position:3} abs=false",
	}
	if len(got) != len(want) {
		t.Errorf("samples = %v", got)
	}
	for rel, w := range want {
		if got[rel] != w {
			t.Errorf("%s:\n got %s\nwant %s", rel, got[rel], w)
		}
	}
	// Blanks are counted on each source alone, not on the columns, and never
	// the narrator. The tags: the filed and loose books' series (1 + 1), the
	// untagged book's all three. The folders: the filed book nothing, the loose
	// file its author and series, the untagged book its series (0 + 2 + 1). The
	// folders say more here, so they are suggested.
	if want := (PreviewBlanks{Tags: 5, Path: 3}); p.Blanks != want {
		t.Errorf("blanks = %+v, want %+v", p.Blanks, want)
	}
	if p.Suggested != catalog.MetadataFromPath {
		t.Errorf("suggested = %q, want path", p.Suggested)
	}
	if p.Root != root {
		t.Errorf("root = %q, want %q", p.Root, root)
	}
}

// Well-tagged loose files at the root named by track: the folders say nothing
// (a track label is no title), so the tags are suggested.
func TestPreviewSuggestsRichTags(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Track 1.m4b"))
	copyChapteredM4B(t, filepath.Join(root, "Track 2.m4b"))
	p, err := previewScanner().Preview(t.Context(), root, nil, PreviewLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// Tags: each book's series. Folders: all three of each.
	if want := (PreviewBlanks{Tags: 2, Path: 6}); p.Blanks != want || p.Suggested != catalog.MetadataFromTags {
		t.Errorf("blanks = %+v, suggested %q; want %+v and tags", p.Blanks, p.Suggested, want)
	}
}

// blanks counts a reading's empty names, the position only beside a series.
func TestPreviewBlanks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		n    catalog.ScannedNames
		want int
	}{
		{catalog.ScannedNames{}, 3},
		{catalog.ScannedNames{Narrator: "N"}, 3},
		{catalog.ScannedNames{Title: "T", Author: "A"}, 1},
		{catalog.ScannedNames{Title: "T", Author: "A", Narrator: "N", Series: "S"}, 1},
		{catalog.ScannedNames{Title: "T", Author: "A", Narrator: "N", Series: "S", Position: "2"}, 0},
	} {
		if got := blanks(tc.n); got != tc.want {
			t.Errorf("blanks(%+v) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// The walk stops at its folder cap and says so; the samples are spread evenly
// over the books it found, in path order; the ignore rules apply, and a bad one
// is refused.
func TestPreviewCapSamplesIgnore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := range 10 {
		writeFile(t, root, fmt.Sprintf("Author/Book %02d/a.mp3", i), []byte("x"))
	}
	s := previewScanner()
	p, err := s.Preview(t.Context(), root, nil, PreviewLimits{Samples: 3})
	if err != nil {
		t.Fatal(err)
	}
	if p.Counts.Capped || p.Counts.FolderBooks != 10 {
		t.Fatalf("uncapped counts = %+v", p.Counts)
	}
	var rels []string
	for _, x := range p.Samples {
		rels = append(rels, x.RelPath)
	}
	if got, want := strings.Join(rels, ","), "Author/Book 00,Author/Book 03,Author/Book 06"; got != want {
		t.Errorf("samples = %s, want %s", got, want)
	}

	// The root, Author and four book folders.
	p, err = s.Preview(t.Context(), root, nil, PreviewLimits{MaxDirs: 6})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Counts.Capped || p.Counts.FolderBooks != 4 || len(p.Samples) != 4 {
		t.Errorf("capped counts = %+v, %d samples; want capped with 4 books", p.Counts, len(p.Samples))
	}

	p, err = s.Preview(t.Context(), root, []string{"Book 0[0-4]/"}, PreviewLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Counts.FolderBooks != 5 {
		t.Errorf("with ignore rules: %d books, want 5", p.Counts.FolderBooks)
	}
	if _, err := s.Preview(t.Context(), root, []string{"Book [0-"}, PreviewLimits{}); !errors.Is(err, ErrInvalidIgnore) {
		t.Errorf("a bad ignore rule = %v, want ErrInvalidIgnore", err)
	}

	// An empty folder previews as nothing, not an error.
	p, err = s.Preview(t.Context(), t.TempDir(), nil, PreviewLimits{})
	if err != nil || p.Counts != (PreviewCounts{}) || len(p.Samples) != 0 || p.Suggested != catalog.MetadataFromTags {
		t.Errorf("empty folder = %+v, %v", p, err)
	}
}

// CheckRoot takes an absolute folder the server can list, cleaned, and refuses
// a relative path, a missing one and a file.
func TestCheckRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "file.mp3", []byte("x"))
	if got, err := CheckRoot(dir + "/./"); err != nil || got != dir {
		t.Errorf("CheckRoot(dir) = %q, %v; want %q", got, err, dir)
	}
	for _, tc := range []struct {
		root string
		want error
	}{
		{"relative/books", ErrNotAbsolute},
		{filepath.Join(dir, "missing"), ErrRootUnreadable},
		{filepath.Join(dir, "file.mp3"), ErrRootUnreadable},
	} {
		if _, err := CheckRoot(tc.root); !errors.Is(err, tc.want) {
			t.Errorf("CheckRoot(%q) = %v, want %v", tc.root, err, tc.want)
		}
	}
	if os.Getuid() != 0 { // root reads anything
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if _, err := CheckRoot(locked); !errors.Is(err, ErrRootUnreadable) {
			t.Errorf("CheckRoot(unreadable) = %v, want ErrRootUnreadable", err)
		}
	}
}

// The sample reads run under the preview's overall time: a read still probing
// when it runs out is ended (its ffprobe killed) and left out, none starts
// after it, and the walk's counts stay whole and uncapped (capped says only
// that the walk stopped, the folder step's "at least this many").
func TestPreviewTimeBudget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ffprobe is a shell script")
	}
	root := t.TempDir()
	for i := range 3 {
		writeFile(t, root, fmt.Sprintf("Author/Book %d/a.mp3", i), []byte("x"))
	}
	slow := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewScanner(nil, slow, slog.Default())
	start := time.Now()
	p, err := s.Preview(t.Context(), root, nil, PreviewLimits{MaxTime: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the preview took %v: its probes outlived its time", took)
	}
	if p.Counts.Capped || p.Counts.FolderBooks != 3 || p.Counts.AudioFiles != 3 || len(p.Samples) != 0 {
		t.Errorf("counts %+v with %d samples; want uncapped, 3 books, no samples", p.Counts, len(p.Samples))
	}
}
