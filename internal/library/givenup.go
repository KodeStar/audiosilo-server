package library

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// givenUp is the first file beside a book's audio (its metadata.json, an
// attachment, its ebook, its cover) whose look or read the scan gave up on
// (metadata.GaveUp: on a share that stopped answering, through
// metadata.Bounded), library-relative. A book with one is kept as stored, that
// file's problem recorded (keep), as a book whose audio read was cut short is:
// what the file holds is unknown, not "none". A nil givenUp notes nothing (a
// backfill, whose caller keeps what is stored on its own).
type givenUp struct {
	file string
	err  error
}

// note keeps file (library-relative) and err when err is a call given up on
// and none was noted before.
func (g *givenUp) note(file string, err error) {
	if g != nil && g.err == nil && metadata.GaveUp(err) {
		g.file, g.err = filepath.ToSlash(file), err
	}
}

// keep marks b, just read, kept as stored when g noted a file (ReadCutShort,
// never LengthOnly: it is not a length the file set rule can settle), and
// records that file's problem over one that would not bring the next scan back
// (problemCleared asks the file again), unless b's audio read was cut short
// itself (its own problem is that).
func (g *givenUp) keep(b *catalog.Book) {
	if g == nil || g.err == nil {
		return
	}
	if !b.ReadCutShort || b.LengthOnly {
		b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = problemUnreadable, g.file, gaveUpDetail(g.err)
	}
	b.ReadCutShort, b.LengthOnly = true, false
}

// gaveUpDetail is the Health detail for a file read given up on with err: a
// read the share refused, or one that did not answer in time.
func gaveUpDetail(err error) string {
	if errors.Is(err, metadata.ErrReadRefused) {
		return readRefusedDetail
	}
	return tagsUnreadDetail
}

// unansweredDetail is the Health detail for a folder or book look given up on
// with err: the share's own words for a fault (metadata.Faulted: a permission,
// EIO), else gaveUpDetail's.
func unansweredDetail(err error) string {
	if cause, ok := metadata.Faulted(err); ok {
		return fsaccess.Cause(cause)
	}
	return gaveUpDetail(err)
}

// noteUnanswered records an unreadable problem (catalog.NoteUnreadablePath) on
// each indexed book a scan's discovery could not look at (unanswered: the
// folders whose listing or files it gave up on, discoverAuto) and so kept as
// stored: one at or below such a folder, or a joined book one of whose discs it
// is (dropUnanswered). A book found (keep), moved, or with a problem recorded
// already is left alone. The next scan that finds the book rechecks the problem
// (problemCleared) and reads the book again once it answers, as after a Re-read
// that gave up.
func (s *Scanner) noteUnanswered(ctx context.Context, lib catalog.Library, sigs map[string]catalog.Signature, keep map[string]bool, moves map[string]string, unanswered map[string]error) {
	if len(unanswered) == 0 {
		return
	}
	for rel, sig := range sigs {
		if _, moved := moves[rel]; keep[rel] || moved || sig.ScanError != "" {
			continue
		}
		uerr, ok := unansweredAt(rel, unanswered)
		if !ok {
			continue
		}
		if err := s.cat.NoteUnreadablePath(ctx, lib, rel, unansweredDetail(uerr)); err != nil {
			s.log.Warn("record an unanswered book's problem failed", "library", lib.Name, "path", rel, "err", err)
			return
		}
	}
}

// unansweredAt is the error of the unanswered folder a book at rel was not
// found for: the folder itself or one above it, else one below it (a disc of
// the joined book at rel).
func unansweredAt(rel string, unanswered map[string]error) (error, bool) {
	for p := rel; ; p = dirOf(p) {
		if err, ok := unanswered[p]; ok {
			return err, true
		}
		if p == "" {
			break
		}
	}
	for dir, err := range unanswered {
		if strings.HasPrefix(dir, rel+"/") {
			return err, true
		}
	}
	return nil, false
}
