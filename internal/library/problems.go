package library

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// What the scanner notices about a book's files for the Health page: read
// problems (books.scan_error) and folders whose parts look like several books
// (books.suspect_parts).

// Read problem codes (books.scan_error).
const (
	problemUnreadable = catalog.ProblemUnreadable  // a file couldn't be opened
	problemEmptyFile  = "empty_file"               // a file is 0 bytes
	problemProbe      = catalog.ProblemProbeFailed // ffprobe couldn't read a file
)

// noteProblem records the first read problem found among a book's files: file is
// the library-relative path of the file just read, size its size, md what reading
// it gave. (The first part whose read was cut short goes over an earlier part's
// problem: buildMultiFileChapters.)
func noteProblem(b *catalog.Book, file string, size int64, md *metadata.Metadata) {
	if b.ScanError != "" {
		return
	}
	switch {
	case size == 0:
		b.ScanError = problemEmptyFile
	case md != nil && md.OpenErr != nil:
		b.ScanError, b.ScanErrorDetail = problemUnreadable, fsaccess.Cause(md.OpenErr)
	case md.ReadRefused():
		// Not read at all: too many reads hang already (metadata.ReadRefused).
		b.ScanError, b.ScanErrorDetail = problemUnreadable, readRefusedDetail
	case md.TagsUnread():
		// Opened but not read in time (a share that stopped answering): the same
		// problem with ffprobe or without (ffprobe may still have read its
		// length), so the next scan looks again (problemCleared).
		b.ScanError, b.ScanErrorDetail = problemUnreadable, tagsUnreadDetail
	case md.ProbeTimedOut():
		// With no ffprobe, its length's in-process read ran out of time even
		// alone (readCutShort): the stored book is kept, so it needs a problem
		// that says why, and that the next scan looks at again.
		b.ScanError, b.ScanErrorDetail = problemUnreadable, tagsUnreadDetail
	case md != nil && errors.Is(md.ProbeErr, metadata.ErrProbeTimeout):
		b.ScanError, b.ScanErrorDetail = catalog.ProblemProbeTimeout, md.ProbeErr.Error()
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
// (most of a no-change scan of a library holding a damaged file). A problem
// with a file beside the audio (a metadata.json, an attachment, an ebook or a
// cover a read gave up on: givenUp) has cleared once the file, or what stands
// there now, answers the kind of call that gave up (sideAnswers; gone counts
// too): the re-index reads it again, and records it again should it still not
// answer.
func (s *Scanner) problemCleared(ctx context.Context, lib catalog.Library, sig catalog.Signature, slots *readSlots) bool {
	if !recheckable(sig) {
		return false
	}
	abs := absOf(lib, sig.ScanErrorFile)
	if !metadata.IsAudio(abs) {
		return sideAnswers(ctx, lib.Root, abs, s.probeTimeout)
	}
	probe := catalog.ScanProblemKind(sig.ScanError) == problemProbe
	var at *fileState
	if probe {
		// The file's own state, before the read: a book's is a folder's newest
		// part and total size, which a part put back as it was leaves unchanged.
		if info, err := metadata.StatBelowBounded(ctx, lib.Root, abs, s.probeTimeout); err == nil {
			at = &fileState{mtime: info.ModTime().Unix(), size: info.Size()}
			if s.wasRejected(abs, *at) {
				return false
			}
		}
	}
	md := slots.extract(ctx, lib.Root, abs, s.ffprobePath)
	if md == nil {
		return false
	}
	s.noteRejected(abs, at, md)
	// A read cut short again (readCutShort: its length still out of time
	// included) has not cleared: the full re-index would only keep the book.
	if md.OpenErr == nil && !readCutShort(md) && (!probe || md.ProbeErr == nil) {
		return true
	}
	// A read cut short before (a share that hung) that is answered now with
	// a lasting problem (a permission on a share that came back): the
	// re-index records that one (the book kept as stored: openFaulted), so
	// the time-out isn't left listed for good.
	return cutShort(sig.ScanErrorDetail) && md.OpenErr != nil && !readCutShort(md)
}

// sideAnswers reports whether the file or folder at abs beside a book's audio,
// whose look or read a scan gave up on (givenUp), answers the same kind of
// bounded call now: a folder its listing (coverLook.find's), a cover image its
// stat (coverLook.find and LookArt only look at one), any other file (a
// metadata.json, an attachment, an ebook) a read of its last bytes (all of a
// small file, an EPUB's zip directory, where checkEbook reads). A stat
// alone would not do for those: on NFS or FUSE it can answer from a cache
// while reads still hang, and every scan would then read the whole book again
// only to wait out the same read. Any lasting answer counts: gone, no longer a
// regular file or folder, or a lasting error (a permission on a share that came
// back); the re-index decides what it is. A fault a retry may get past
// (metadata.TransientFault: EIO, ENOTCONN) is no answer: the re-index would meet it on
// the same look, and a cover look reading it as "no cover" would drop the
// stored one. Every look is through the library's root (metadata.StatBelow,
// ReadDirBelow, OpenRegularBelow), never out of it by a link swapped in since.
func sideAnswers(ctx context.Context, root, abs string, timeout time.Duration) bool {
	// One bounded call, the stat and the look it leads to: a share that
	// stopped answering costs timeout once, and holds one of its mount's slots.
	_, err := metadata.Bounded(ctx, abs, timeout, func() (struct{}, error) {
		info, err := metadata.StatBelow(root, abs)
		switch {
		case err != nil:
		case info.IsDir():
			var d *metadata.DirBelow
			if d, err = metadata.ReadDirBelow(root, abs); err == nil {
				_ = d.Close()
			}
		case imageExts[strings.ToLower(filepath.Ext(abs))], !info.Mode().IsRegular():
		default:
			var f *os.File
			if f, info, err = metadata.OpenRegularBelow(root, abs); err == nil {
				_, err = f.ReadAt(make([]byte, sideProbeBytes), max(0, info.Size()-sideProbeBytes))
				_ = f.Close()
				if errors.Is(err, io.EOF) {
					err = nil
				}
			}
		}
		return struct{}{}, err
	}, nil)
	// Any lasting answer counts (a permission on a share that came back
	// included: the re-index records what it is); a look given up on again,
	// or a share's fault a retry may get past, doesn't.
	return !metadata.Unanswered(ctx, err) && !metadata.TransientFault(err)
}

// sideProbeBytes is how much of a file beside the audio sideAnswers reads.
const sideProbeBytes = 4 << 10

// tagsUnreadDetail is the detail of a file that opened but wasn't read in
// time, and readRefusedDetail of one not read at all, as too many earlier
// reads from its drive or share (its mount, on any library) are stuck (Health:
// "<file> couldn't be read: <detail>").
const (
	tagsUnreadDetail  = "reading it timed out"
	readRefusedDetail = "too many earlier reads from the same drive or share are stuck, so it wasn't tried"
)

// cutShort reports whether detail is a read problem's that says the file's
// read was cut short or refused (tagsUnreadDetail, readRefusedDetail), not
// what the file is: one a later read may answer otherwise.
func cutShort(detail string) bool { return detail == tagsUnreadDetail || detail == readRefusedDetail }

// keptAsStored reports whether sig records a read problem that a read meeting
// it again keeps the stored book over (catalog.Book.ReadCutShort), writing
// nothing new of it: any unreadable one (a read cut short or refused, an open
// a share failed, a file that is no regular file) and an ffprobe time-out.
// Until problemCleared finds the file answers, reading the book again for
// another reason (a cover that appeared beside it) would write nothing, on
// every scan.
func keptAsStored(sig catalog.Signature) bool {
	return sig.ScanError == problemUnreadable ||
		catalog.IsProbeTimeout(sig.ScanError, sig.ScanErrorDetail)
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
	return sig.ScanErrorFile != "" && (sig.ScanError == problemUnreadable || catalog.ScanProblemKind(sig.ScanError) == problemProbe)
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
// passing. ok is false when a part can't be read, or its tag read was cut short
// (left for the next scan): each read is bounded by ctx and timeout, and one on
// a share that stopped answering is cut short there, the rest not read.
func suspectFromTags(ctx context.Context, lib catalog.Library, b *catalog.Book, timeout time.Duration) (n int, ok bool) {
	parts := make([]partFacts, 0, len(b.Files))
	for _, f := range b.Files {
		md := metadata.ExtractTags(ctx, lib.Root, absOf(lib, f.RelPath), timeout)
		if md.OpenErr != nil || md.TagsUnread() {
			return 0, false
		}
		parts = append(parts, partFacts{title: partName(f.RelPath, md), duration: minSuspectPart})
	}
	return suspectParts(parts), true
}
