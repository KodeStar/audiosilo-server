package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// testHead is a map head made from ebook shaA and files.
func testHead(files ...metadata.ReadalongFile) *metadata.ReadalongHead {
	return &metadata.ReadalongHead{Format: metadata.ReadalongFormat, Ebook: metadata.ReadalongEbook{SHA256: shaA},
		Audio:       metadata.ReadalongAudio{Files: files},
		GeneratedAt: "2026-11-02T10:11:12Z", Coverage: metadata.ReadalongCoverage{Text: 0.996, Audio: 0.987}}
}

// TestReadalongState: ready only for the ebook the map was made from and the
// same audio (count, paths as a set, each length within 0.05 s); the ebook is
// checked first.
func TestReadalongState(t *testing.T) {
	t.Parallel()
	f := func(p string, d float64) metadata.ReadalongFile { return metadata.ReadalongFile{Path: p, Duration: d} }
	head := testHead(f("01.mp3", 100), f("02.mp3", 200))
	ready := `{"state":"ready","coverage":0.996,"generated_at":"2026-11-02T10:11:12Z"}`
	audio := `{"state":"stale","reason":"audio_changed"}`
	ebook := `{"state":"stale","reason":"ebook_changed"}`
	cases := []struct {
		name  string
		sha   string
		files []metadata.ReadalongFile
		want  string
	}{
		{"same", shaA, []metadata.ReadalongFile{f("01.mp3", 100), f("02.mp3", 200)}, ready},
		{"another order", shaA, []metadata.ReadalongFile{f("02.mp3", 200), f("01.mp3", 100)}, ready},
		{"the digest in capitals", strings.ToUpper(shaA), []metadata.ReadalongFile{f("01.mp3", 100), f("02.mp3", 200)}, ready},
		{"within the slack", shaA, []metadata.ReadalongFile{f("01.mp3", 100.05), f("02.mp3", 199.96)}, ready},
		{"a length off", shaA, []metadata.ReadalongFile{f("01.mp3", 99), f("02.mp3", 200)}, audio},
		{"just past the slack", shaA, []metadata.ReadalongFile{f("01.mp3", 100.051), f("02.mp3", 200)}, audio},
		{"a file gone", shaA, []metadata.ReadalongFile{f("01.mp3", 100)}, audio},
		{"a file added", shaA, []metadata.ReadalongFile{f("01.mp3", 100), f("02.mp3", 200), f("03.mp3", 1)}, audio},
		{"a file renamed", shaA, []metadata.ReadalongFile{f("01.mp3", 100), f("2.mp3", 200)}, audio},
		{"the book's files repeat", shaA, []metadata.ReadalongFile{f("01.mp3", 100), f("01.mp3", 100)}, audio},
		{"another ebook", shaB, []metadata.ReadalongFile{f("01.mp3", 100), f("02.mp3", 200)}, ebook},
		{"another ebook and audio", shaB, []metadata.ReadalongFile{f("01.mp3", 1)}, ebook},
	}
	for _, tc := range cases {
		got, _ := json.Marshal(ReadalongState(head, tc.sha, tc.files))
		if string(got) != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// A map listing a file twice is never the book's.
	if st := ReadalongState(testHead(f("01.mp3", 100), f("01.mp3", 100)), shaA,
		[]metadata.ReadalongFile{f("01.mp3", 100), f("02.mp3", 100)}); st.State != ReadalongStale {
		t.Errorf("a repeated map file: %+v", st)
	}
	if ReadalongState(nil, shaA, nil) != nil {
		t.Error("no map: want no state")
	}
}

// TestBookAudioFiles: paths relative to the book, a single-file book its own
// one file by its base name, a book on the library root unprefixed.
func TestBookAudioFiles(t *testing.T) {
	t.Parallel()
	got := BookAudioFiles("A/Book", true, 0, []BookFile{{RelPath: "A/Book/CD1/01.mp3", Duration: 1}, {RelPath: "A/Book/02.mp3", Duration: 2}})
	if want := []metadata.ReadalongFile{{Path: "CD1/01.mp3", Duration: 1}, {Path: "02.mp3", Duration: 2}}; !equalFiles(got, want) {
		t.Errorf("folder book = %+v", got)
	}
	if got := BookAudioFiles("A/Loose.m4b", false, 42, nil); !equalFiles(got, []metadata.ReadalongFile{{Path: "Loose.m4b", Duration: 42}}) {
		t.Errorf("single-file book = %+v", got)
	}
	if got := BookAudioFiles(".", true, 0, []BookFile{{RelPath: "01.mp3", Duration: 1}}); !equalFiles(got, []metadata.ReadalongFile{{Path: "01.mp3", Duration: 1}}) {
		t.Errorf("root book = %+v", got)
	}
}

func equalFiles(a, b []metadata.ReadalongFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadalongIndex: a stored map's head reads back, a map is never generated
// content (Book.generated stays off), only books with a map and an ebook whose
// digest isn't current are due for hashing, and a digest is recorded only for
// the ebook the row still holds.
func TestReadalongIndex(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	head := testHead(metadata.ReadalongFile{Path: "a.mp3", Duration: 1})
	for _, b := range []Book{
		{RelPath: "Mapped", EbookFile: EbookReading{Path: "Mapped/b.epub", Sig: "10:1:b.epub", Size: 10},
			Attachments: AttachmentsReading{Sig: "readalong.json:5:1", Files: metadata.AttachmentFiles{Readalong: head}}},
		{RelPath: "NoEbook", Attachments: AttachmentsReading{Sig: "readalong.json:5:1", Files: metadata.AttachmentFiles{Readalong: head}}},
		{RelPath: "NoMap", EbookFile: EbookReading{Path: "NoMap/b.epub", Sig: "10:1:b.epub", Size: 10}},
	} {
		b.LibraryID, b.Title, b.IsFolder = lib.ID, b.RelPath, true
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.BookReadalongAt(ctx, lib.ID, "Mapped")
	if err != nil || got.Head == nil || got.Head.Ebook.SHA256 != shaA || got.DigestCurrent || got.EbookSHA256 != "" {
		t.Fatalf("indexed = %+v, %v", got, err)
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, "Mapped"); b.Generated || b.Readalong != nil {
		t.Errorf("a map: generated = %v, readalong = %+v; want neither on a read", b.Generated, b.Readalong)
	}
	if got, _ := c.BookReadalongAt(ctx, lib.ID, "NoMap"); got.Head != nil {
		t.Errorf("no map: %+v", got)
	}
	if _, err := c.BookReadalongAt(ctx, lib.ID, "Nobody"); err != ErrNotFound {
		t.Errorf("no book: %v", err)
	}
	due, err := c.ReadalongEbooksDue(ctx, lib.ID, "")
	if err != nil || len(due) != 1 || due[0] != (EbookDue{BookPath: "Mapped", EbookPath: "Mapped/b.epub", IsFolder: true}) {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if due, _ := c.ReadalongEbooksDue(ctx, lib.ID, "NoMap"); len(due) != 0 {
		t.Errorf("due for NoMap = %+v", due)
	}
	// A digest of a file the index moved on from is never recorded.
	if err := c.SetEbookDigest(ctx, lib.ID, "Mapped", "11:1:b.epub", shaB); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.BookReadalongAt(ctx, lib.ID, "Mapped"); got.DigestCurrent || got.EbookSHA256 != "" {
		t.Errorf("a digest of another file: %+v", got)
	}
	if err := c.SetEbookDigest(ctx, lib.ID, "Mapped", "10:1:b.epub", shaA); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.BookReadalongAt(ctx, lib.ID, "Mapped"); !got.DigestCurrent || got.EbookSHA256 != shaA {
		t.Errorf("recorded digest: %+v", got)
	}
	if due, _ := c.ReadalongEbooksDue(ctx, lib.ID, ""); len(due) != 0 {
		t.Errorf("due after hashing = %+v", due)
	}
	// The ebook changes: the digest is no longer current, and it is due again.
	if err := c.SetEbooks(ctx, lib.ID, map[string]EbookReading{"Mapped": {Path: "Mapped/b.epub", Sig: "12:2:b.epub", Size: 12}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.BookReadalongAt(ctx, lib.ID, "Mapped"); got.DigestCurrent {
		t.Errorf("after the ebook changed: %+v", got)
	}
	if due, _ := c.ReadalongEbooksDue(ctx, lib.ID, "Mapped"); len(due) != 1 {
		t.Errorf("due after the ebook changed = %+v", due)
	}
}
