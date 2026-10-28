package library

import (
	"errors"
	"io/fs"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// What the scanner notices about a book's files for the Health page: read
// problems (books.scan_error) and folders whose parts look like several books
// (books.suspect_parts).

// Read problem codes (books.scan_error).
const (
	problemUnreadable = "unreadable"   // a file couldn't be opened
	problemEmptyFile  = "empty_file"   // a file is 0 bytes
	problemProbe      = "probe_failed" // ffprobe couldn't read a file
)

// noteProblem records the first read problem found among a book's files: file is
// the library-relative path of the file just read, size its size, md what reading
// it gave.
func noteProblem(b *catalog.Book, file string, size int64, md *metadata.Metadata) {
	if b.ScanError != "" {
		return
	}
	switch {
	case size == 0:
		b.ScanError = problemEmptyFile
	case md != nil && md.OpenErr != nil:
		b.ScanError, b.ScanErrorDetail = problemUnreadable, pathErrText(md.OpenErr)
	case md != nil && md.ProbeErr != nil:
		b.ScanError, b.ScanErrorDetail = problemProbe, md.ProbeErr.Error()
	default:
		return
	}
	b.ScanErrorFile = file
}

// problemCleared reports whether a book's recorded read problem has gone: the
// file it names opens now (it was unreadable) or ffprobe reads it (it couldn't).
// A fixed permission or a share that came back changes neither the file's mtime
// nor its size, and a problem on a part other than the first leaves the book's
// duration and codec looking complete, so without this a scan would skip the book
// and the problem would stay listed. It reads only that one file, and not even
// that when this server has seen ffprobe reject the file's data with the book as
// it is now (rejectedFile): the same ffprobe rejects the same bytes again, and
// each run is a process start (most of a no-change scan of a library holding a
// damaged file).
func (s *Scanner) problemCleared(lib catalog.Library, sig catalog.Signature) bool {
	if sig.ScanErrorFile == "" || (sig.ScanError != problemUnreadable && sig.ScanError != problemProbe) {
		return false
	}
	file := rejectedFile{library: lib.ID, file: sig.ScanErrorFile}
	book := bookState{mtime: sig.MTime, size: sig.Size}
	probe := sig.ScanError == problemProbe
	if probe && s.wasRejected(file, book) {
		return false
	}
	md, _ := metadata.Extract(absOf(lib, sig.ScanErrorFile), s.ffprobePath)
	if probe {
		s.noteRejected(file, book, md)
	}
	return md.OpenErr == nil && (!probe || md.ProbeErr == nil)
}

// rejectedFile is a file ffprobe rejected (metadata.ErrProbeRejected), kept
// with its book's bookState then: a scan that finds the book in the same state
// takes it to hold the same bytes, as it does for every book it finds unchanged.
type rejectedFile struct {
	library int64
	file    string // library-relative
}

// bookState is a book's mtime and size.
type bookState struct{ mtime, size int64 }

// noteRejected remembers file with its book in state book when md says ffprobe
// rejected the file's data (not a time-out, an exec failure or a read error:
// those may pass), replacing what was remembered of it before. Remembered for
// the server's life only, so a restart (perhaps with another ffprobe) looks
// again.
func (s *Scanner) noteRejected(file rejectedFile, book bookState, md *metadata.Metadata) {
	if md == nil || md.OpenErr != nil || !errors.Is(md.ProbeErr, metadata.ErrProbeRejected) {
		return
	}
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	if s.rejected == nil {
		s.rejected = map[rejectedFile]bookState{}
	}
	s.rejected[file] = book
}

// wasRejected reports whether noteRejected remembered file with its book in
// state book.
func (s *Scanner) wasRejected(file rejectedFile, book bookState) bool {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	was, ok := s.rejected[file]
	return ok && was == book
}

// soleFileProblem reports whether a book's one audio file is the file its
// recorded read problem names. Such a book has no duration or codec stored (its
// only file was never probed), so a scan finding it unchanged lets
// problemCleared decide whether to read it again, rather than reading all of it
// on every scan only to record the same problem.
func soleFileProblem(b *catalog.Book, sig catalog.Signature) bool {
	return soleFile(b) && sig.ScanErrorFile == primaryPath(b) &&
		(sig.ScanError == problemUnreadable || sig.ScanError == problemProbe)
}

// soleFile reports whether a book has one audio file (a file book, or a folder
// holding one).
func soleFile(b *catalog.Book) bool { return !b.IsFolder || len(b.Files) == 1 }

// pathErrText is an OS error's own words without the absolute path it names
// ("permission denied", not "open /srv/books/x.mp3: permission denied"): the
// console shows the library-relative file beside it.
func pathErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// Folder-holds-several-books detection. A folder with audio is one book, which is
// right for almost every library; the exception worth flagging is a folder that
// collects whole books ("Series/Book 1.m4b", "Series/Book 2.m4b"). Its parts then
// carry different titles (the album tag, else the file name) and are each
// book-length, where the parts of one book share a title or are chapter-length.

// minSuspectPart is how long every part must be for a folder to look like several
// books.
const minSuspectPart = 60 * 60 // seconds

// partFacts is what the check needs about one part.
type partFacts struct {
	title    string
	duration float64
}

// partName is the title a part claims: its book-title tag, else its file name
// without a leading track number.
func partName(relPath string, md *metadata.Metadata) string {
	if md != nil && strings.TrimSpace(md.Title) != "" && !metadata.IsGenericTitle(md.Title) {
		return md.Title
	}
	return partTitle(relPath)
}

// partMarkers are words that number the parts of one book; they and bare numbers
// are dropped before titles are compared.
var partMarkers = map[string]bool{"part": true, "pt": true, "of": true, "cd": true, "disc": true, "disk": true, "track": true}

// titleKey reduces a part title to what tells books apart: lower case, letters
// and digits only, part markers and numbers dropped. "" when nothing is left.
func titleKey(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	kept := words[:0]
	for _, w := range words {
		// "Part1", "CD2" and bare numbers number the parts of one book.
		stem := strings.TrimRightFunc(w, unicode.IsDigit)
		if stem == "" || partMarkers[stem] {
			continue
		}
		kept = append(kept, w)
	}
	return strings.Join(kept, " ")
}

// suspectParts returns how many distinct books a folder's parts look like, or 0
// when they look like one: at least two parts, every one book-length (a part of
// unknown length, without ffprobe, can't tell), with at least two different titles.
func suspectParts(parts []partFacts) int {
	if len(parts) < 2 {
		return 0
	}
	titles := map[string]bool{}
	for _, p := range parts {
		if p.duration < minSuspectPart {
			return 0
		}
		if k := titleKey(p.title); k != "" {
			titles[k] = true
		}
	}
	if len(titles) < 2 {
		return 0
	}
	return len(titles)
}

// suspectFromTags runs the check for a folder book indexed before it existed,
// reading only tags (no ffprobe). Migration 0017 left unchecked only the folder
// books whose parts are all at least an hour long, so the lengths are taken as
// passing. ok is false when a part can't be read (left for the next scan).
func suspectFromTags(lib catalog.Library, b *catalog.Book) (n int, ok bool) {
	parts := make([]partFacts, 0, len(b.Files))
	for _, f := range b.Files {
		md, _ := metadata.Extract(absOf(lib, f.RelPath), "")
		if md.OpenErr != nil {
			return 0, false
		}
		parts = append(parts, partFacts{title: partName(f.RelPath, md), duration: minSuspectPart})
	}
	return suspectParts(parts), true
}
