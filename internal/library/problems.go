package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
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
// and the problem would stay listed. It reads only that one file, with the scan's
// slots under its ctx as enrich reads (a read the scan stopped answers false: the
// scan ends at its next check), and not even that when this server has seen
// ffprobe reject the file's data with the file as it is now (noteRejected): the
// same ffprobe rejects the same bytes again, and each run is a process start
// (most of a no-change scan of a library holding a damaged file).
func (s *Scanner) problemCleared(ctx context.Context, lib catalog.Library, sig catalog.Signature, slots *readSlots) bool {
	if !recheckable(sig) {
		return false
	}
	abs := absOf(lib, sig.ScanErrorFile)
	probe := sig.ScanError == problemProbe
	var at *fileState
	if probe {
		// The file's own state, before the read: a book's is a folder's newest
		// part and total size, which a part put back as it was leaves unchanged.
		if info, err := os.Stat(abs); err == nil {
			at = &fileState{mtime: info.ModTime().Unix(), size: info.Size()}
			if s.wasRejected(abs, *at) {
				return false
			}
		}
	}
	md := slots.extract(ctx, abs, s.ffprobePath)
	if md == nil {
		return false
	}
	s.noteRejected(abs, at, md)
	return md.OpenErr == nil && (!probe || md.ProbeErr == nil)
}

// fileState is a file's mtime (Unix seconds) and size: with its absolute path,
// what a rejection of its data is remembered by (noteRejected). A scan that
// finds the file in the same state takes it to hold the same bytes, as it does
// for every book it finds unchanged.
type fileState struct{ mtime, size int64 }

// noteRejected keeps what md, a reading of the file at abs, says for
// problemCleared. A rejection of its data (metadata.ErrProbeRejected; not a
// time-out, an exec failure or a read error, which may pass) is remembered with
// the file's state from before the read (at; nil when the reader doesn't know
// it, and nothing is remembered then), replacing what was. Any other reading
// forgets what was remembered of the file: it has read otherwise since. A read
// stopped by its scan (nil) says nothing. Remembered for the server's life only,
// so a restart (perhaps with another ffprobe) looks again.
func (s *Scanner) noteRejected(abs string, at *fileState, md *metadata.Metadata) {
	if md == nil {
		return
	}
	rejected := md.OpenErr == nil && errors.Is(md.ProbeErr, metadata.ErrProbeRejected)
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	switch {
	case !rejected:
		delete(s.rejected, abs)
	case at != nil:
		if s.rejected == nil {
			s.rejected = map[string]fileState{}
		}
		s.rejected[abs] = *at
	}
}

// wasRejected reports whether noteRejected remembered the file at abs in state
// at.
func (s *Scanner) wasRejected(abs string, at fileState) bool {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	was, ok := s.rejected[abs]
	return ok && was == at
}

// recheckable reports whether sig records a read problem with one file that
// problemCleared can look at again.
func recheckable(sig catalog.Signature) bool {
	return sig.ScanErrorFile != "" && (sig.ScanError == problemUnreadable || sig.ScanError == problemProbe)
}

// soleFileProblem reports whether a book's one audio file is the file its
// recorded read problem names. Such a book has no duration or codec stored (its
// only file was never probed), so a scan finding it unchanged lets
// problemCleared decide whether to read it again, rather than reading all of it
// on every scan only to record the same problem.
func soleFileProblem(b *catalog.Book, sig catalog.Signature) bool {
	return soleFile(b) && sig.ScanErrorFile == primaryPath(b) && recheckable(sig)
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
