package metadata

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ChapterList is a book's chapters.json (BRIDGE-PLAN.md Phase 1d): the chapters
// the Bridge found in the narration of a book with no chapter markers, on the
// whole-book timeline (the shape of a community recording's chapter list), with
// the files they were found on. The server uses it as a chapter source only
// while the book's files are still those (catalog's applyChapterSource), so a
// list for other audio never stands in.
type ChapterList struct {
	Version int `json:"version"`
	// Generator, GeneratedAt and From say what made it (the Bridge, when, and
	// from what: "narration"); cleaned like a provenance value, never a reason
	// to refuse the file.
	Generator   string             `json:"generator,omitempty"`
	GeneratedAt string             `json:"generated_at,omitempty"`
	From        string             `json:"from,omitempty"`
	Files       []ChapterListFile  `json:"files"`
	Chapters    []ChapterListEntry `json:"chapters"`
}

// ChapterListFile is one of the files a ChapterList was found on, in play
// order: its path relative to the book's folder (a single-file book's is its
// file name, the folder being the one its .audiosilo folder is in), its size in
// bytes and its length.
type ChapterListFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	DurationMS int64  `json:"duration_ms"`
}

// ChapterListEntry is one chapter: its title (as spoken) and where it starts
// and how long it runs on the whole-book timeline.
type ChapterListEntry struct {
	Title    string `json:"title"`
	StartMS  int64  `json:"start_ms"`
	LengthMS int64  `json:"length_ms"`
}

// The bounds a chapters.json is checked against.
const (
	// ChapterListVersion is the one format version the server reads.
	ChapterListVersion = 1
	// A list has at least two chapters (one chapter is no chapters: the Bridge
	// writes no file) and at most maxChapterListEntries; at most
	// maxChapterListFiles files.
	minChapterListEntries = 2
	maxChapterListEntries = 2000
	maxChapterListFiles   = 2000
	// maxChapterTitle is the longest title in bytes, maxChapterFilePath the
	// longest file path.
	maxChapterTitle    = 200
	maxChapterFilePath = 4096
	// chapterGapMS is how far a chapter may start from the previous one's end
	// (a rounding of the Bridge's, never a hole or an overlap in the book).
	chapterGapMS = 1000
)

// ParseChapterList decodes and checks a chapters.json: version 1; 2 to 2000
// chapters, each with a title of 1 to 200 bytes once trimmed (stored trimmed)
// with no control character, a start >= 0 (the first at 0, each after the one
// before and within a second of its end) and a length > 0; 1 to 2000 files,
// each with a relative path that names no dot segment (no "..", no hidden
// folder or file) and a size and length >= 0. Unknown fields are allowed. Any
// check that fails fails the file (the Bridge validates what it writes, so a
// failure means the file isn't the Bridge's).
func ParseChapterList(data []byte) (*ChapterList, error) {
	var l ChapterList
	if err := decodeAttachment(data, &l); err != nil {
		return nil, err
	}
	if l.Version != ChapterListVersion {
		return nil, invalidAttachment("chapters version %d", l.Version)
	}
	if n := len(l.Chapters); n < minChapterListEntries || n > maxChapterListEntries {
		return nil, invalidAttachment("%d chapters", n)
	}
	if n := len(l.Files); n == 0 || n > maxChapterListFiles {
		return nil, invalidAttachment("%d files", n)
	}
	for i := range l.Chapters {
		ch := &l.Chapters[i]
		ch.Title = strings.TrimSpace(ch.Title)
		if ch.Title == "" || len(ch.Title) > maxChapterTitle || !utf8.ValidString(ch.Title) ||
			strings.ContainsFunc(ch.Title, unicode.IsControl) {
			return nil, invalidAttachment("chapter %d's title", i)
		}
		if ch.StartMS < 0 || ch.LengthMS <= 0 {
			return nil, invalidAttachment("chapter %d starts at %d ms for %d ms", i, ch.StartMS, ch.LengthMS)
		}
		if i == 0 {
			if ch.StartMS != 0 {
				return nil, invalidAttachment("the first chapter starts at %d ms", ch.StartMS)
			}
			continue
		}
		prev := l.Chapters[i-1]
		if end := prev.StartMS + prev.LengthMS; ch.StartMS <= prev.StartMS || ch.StartMS < end-chapterGapMS || ch.StartMS > end+chapterGapMS {
			return nil, invalidAttachment("chapter %d starts at %d ms, the one before ends at %d ms", i, ch.StartMS, end)
		}
	}
	for i, f := range l.Files {
		if !relativeFilePath(f.Path) {
			return nil, invalidAttachment("file %d's path", i)
		}
		if f.Size < 0 || f.DurationMS < 0 {
			return nil, invalidAttachment("file %d's size or length", i)
		}
	}
	l.Generator, l.From = cleanProvenance(l.Generator), cleanProvenance(l.From)
	l.GeneratedAt = cleanTime(l.GeneratedAt)
	return &l, nil
}

// relativeFilePath reports whether p is a file path relative to a book's
// folder as a chapters.json names one: not empty or too long, valid text with
// no control character or backslash, slash-separated with no empty segment and
// none starting with a dot (".", "..", or a hidden name the scan never reads).
func relativeFilePath(p string) bool {
	if p == "" || len(p) > maxChapterFilePath || !utf8.ValidString(p) ||
		strings.ContainsFunc(p, func(r rune) bool { return unicode.IsControl(r) || r == '\\' }) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}
