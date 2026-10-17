package metadata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chapterListJSON is a chapters.json with the given files and chapters (JSON
// arrays) and the Bridge's other fields.
func chapterListJSON(files, chapters string) string {
	return `{"version":1,"generator":"audiosilo-bridge","generated_at":"2026-10-17T09:00:00+02:00","from":"narration",
		"files":` + files + `,"chapters":` + chapters + `}`
}

const (
	oneFile      = `[{"path":"Book.mp3","size":123,"duration_ms":3600000}]`
	twoChapters  = `[{"title":" Chapter 1 ","start_ms":0,"length_ms":1000000},{"title":"Chapter 2","start_ms":1000000,"length_ms":2600000}]`
	manyChapters = 2000
)

// TestParseChapterList: the Bridge's file is read (unknown fields allowed, a
// byte order mark skipped, titles trimmed, the provenance values cleaned);
// every check that fails fails the file.
func TestParseChapterList(t *testing.T) {
	t.Parallel()
	l, err := ParseChapterList([]byte("\xef\xbb\xbf" + strings.Replace(chapterListJSON(oneFile, twoChapters), `"version":1`, `"version":1,"new_field":true`, 1)))
	if err != nil || l == nil || len(l.Chapters) != 2 || l.Chapters[0].Title != "Chapter 1" || l.Files[0].Path != "Book.mp3" ||
		l.Files[0].Size != 123 || l.Files[0].DurationMS != 3600000 || l.GeneratedAt != "2026-10-17T07:00:00Z" ||
		l.Generator != "audiosilo-bridge" || l.From != "narration" {
		t.Fatalf("valid file = %+v, %v", l, err)
	}
	// A start within a second of the previous end either way is no gap.
	if _, err := ParseChapterList([]byte(chapterListJSON(oneFile,
		`[{"title":"A","start_ms":0,"length_ms":1000},{"title":"B","start_ms":1999,"length_ms":2000},{"title":"C","start_ms":3000,"length_ms":5}]`))); err != nil {
		t.Errorf("starts within a second: %v", err)
	}
	// The bounds themselves pass.
	var b strings.Builder
	for i := range manyChapters {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"title":"%s","start_ms":%d,"length_ms":1000}`, strings.Repeat("t", maxChapterTitle), i*1000)
	}
	if _, err := ParseChapterList([]byte(chapterListJSON(oneFile, "["+b.String()+"]"))); err != nil {
		t.Errorf("2000 chapters of the longest title: %v", err)
	}
	// A bad provenance value costs that value, not the file.
	if l, err := ParseChapterList([]byte(strings.Replace(chapterListJSON(oneFile, twoChapters), `"generated_at":"2026-10-17T09:00:00+02:00"`, `"generated_at":"yesterday"`, 1))); err != nil || l.GeneratedAt != "" {
		t.Errorf("a bad time: %+v, %v", l, err)
	}

	tooMany := "[" + strings.TrimSuffix(strings.Repeat(`{"path":"a.mp3","size":1,"duration_ms":1},`, maxChapterListFiles+1), ",") + "]"
	var over strings.Builder
	for i := range manyChapters + 1 {
		if i > 0 {
			over.WriteByte(',')
		}
		fmt.Fprintf(&over, `{"title":"C","start_ms":%d,"length_ms":1000}`, i*1000)
	}
	file := func(p string) string { return fmt.Sprintf(`[{"path":%q,"size":1,"duration_ms":1}]`, p) }
	chapter := func(title string) string {
		return fmt.Sprintf(`[{"title":%q,"start_ms":0,"length_ms":1000},{"title":"B","start_ms":1000,"length_ms":1000}]`, title)
	}
	for name, body := range map[string]string{
		"not JSON":           `not json`,
		"not an object":      `[]`,
		"no version":         strings.Replace(chapterListJSON(oneFile, twoChapters), `"version":1,`, ``, 1),
		"version 2":          strings.Replace(chapterListJSON(oneFile, twoChapters), `"version":1`, `"version":2`, 1),
		"one chapter":        chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":1000}]`),
		"no chapters":        chapterListJSON(oneFile, `[]`),
		"too many chapters":  chapterListJSON(oneFile, "["+over.String()+"]"),
		"no files":           chapterListJSON(`[]`, twoChapters),
		"too many files":     chapterListJSON(tooMany, twoChapters),
		"blank title":        chapterListJSON(oneFile, chapter("   ")),
		"long title":         chapterListJSON(oneFile, chapter(strings.Repeat("t", maxChapterTitle+1))),
		"a newline in title": chapterListJSON(oneFile, chapter("A\nB")),
		"a tab in title":     chapterListJSON(oneFile, chapter("A\tB")),
		"a C1 control":       chapterListJSON(oneFile, chapter("A\u0085B")),
		"first not at 0":     chapterListJSON(oneFile, `[{"title":"A","start_ms":5,"length_ms":1000},{"title":"B","start_ms":1005,"length_ms":1000}]`),
		"negative start":     chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":1000},{"title":"B","start_ms":-1,"length_ms":1000}]`),
		"zero length":        chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":0},{"title":"B","start_ms":0,"length_ms":1000}]`),
		"negative length":    chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":1000},{"title":"B","start_ms":1000,"length_ms":-5}]`),
		"not ascending":      chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":1000},{"title":"B","start_ms":0,"length_ms":1000}]`),
		"a gap":              chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":1000},{"title":"B","start_ms":2001,"length_ms":1000}]`),
		"an overlap":         chapterListJSON(oneFile, `[{"title":"A","start_ms":0,"length_ms":5000},{"title":"B","start_ms":3999,"length_ms":1000}]`),
		"empty path":         chapterListJSON(file(""), twoChapters),
		"absolute path":      chapterListJSON(file("/etc/passwd"), twoChapters),
		"dot dot":            chapterListJSON(file("../Other/Book.mp3"), twoChapters),
		"dot dot inside":     chapterListJSON(file("CD1/../../x.mp3"), twoChapters),
		"dot segment":        chapterListJSON(file("./Book.mp3"), twoChapters),
		"hidden folder":      chapterListJSON(file(".audiosilo/Book.mp3"), twoChapters),
		"empty segment":      chapterListJSON(file("CD1//01.mp3"), twoChapters),
		"trailing slash":     chapterListJSON(file("CD1/"), twoChapters),
		"backslash":          chapterListJSON(file(`CD1\01.mp3`), twoChapters),
		"control in path":    chapterListJSON(file("a\u0001.mp3"), twoChapters),
		"long path":          chapterListJSON(file(strings.Repeat("p", maxChapterFilePath+1)), twoChapters),
		"negative size":      chapterListJSON(`[{"path":"a.mp3","size":-1,"duration_ms":1}]`, twoChapters),
		"negative duration":  chapterListJSON(`[{"path":"a.mp3","size":1,"duration_ms":-1}]`, twoChapters),
		"wrong type":         chapterListJSON(`[{"path":7,"size":1}]`, twoChapters),
	} {
		if l, err := ParseChapterList([]byte(body)); !errors.Is(err, ErrAttachmentInvalid) {
			t.Errorf("%s: %+v, %v; want ErrAttachmentInvalid", name, l, err)
		}
	}
}

// TestReadChapterList: a regular file is read, a link or one over the size cap
// is unusable.
func TestReadChapterList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := filepath.Join(dir, "chapters.json")
	if err := os.WriteFile(valid, []byte(chapterListJSON(oneFile, twoChapters)), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := ReadChapterList(valid); err != nil || l == nil {
		t.Fatalf("regular file: %+v, %v", l, err)
	}
	link := filepath.Join(dir, "Book.chapters.json")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChapterList(link); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("a link: %v, want ErrAttachmentInvalid", err)
	}
	big := filepath.Join(dir, "big.chapters.json")
	if err := os.WriteFile(big, []byte(strings.Replace(chapterListJSON(oneFile, twoChapters), `"version":1`,
		`"version":1,"pad":"`+strings.Repeat("x", maxAttachmentFile)+`"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChapterList(big); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("too large: %v, want ErrAttachmentInvalid", err)
	}
}
