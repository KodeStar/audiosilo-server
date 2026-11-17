package library

import (
	"errors"
	"path/filepath"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
		detail := tagsUnreadDetail
		if errors.Is(g.err, metadata.ErrReadRefused) {
			detail = readRefusedDetail
		}
		b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = problemUnreadable, g.file, detail
	}
	b.ReadCutShort, b.LengthOnly = true, false
}
