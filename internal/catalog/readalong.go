package catalog

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A book's read-along map (Bridge Phase 3c): the Bridge's .audiosilo/readalong.json,
// where each sentence of the book's ebook is spoken in its audio. The scan stores
// its checked head (book_attachments.readalong, migration 0058: never the
// sections), and hashes the ebook of a book that has one (books.ebook_sha256,
// computed for the ebook_sig in ebook_hashed_sig), so ReadalongState can tell a
// fresh map from one whose ebook or audio changed since it was made.

// The states and stale reasons of Book.readalong.
const (
	ReadalongReady     = "ready"
	ReadalongStale     = "stale"
	ReasonEbookChanged = "ebook_changed"
	ReasonAudioChanged = "audio_changed"
)

// readalongDurationSlack is how far a file's length may differ from the one
// the map was made with before the map is stale: ffprobe's lengths of one file
// agree far closer than this, and a trimmed second is far beyond it.
const readalongDurationSlack = 0.05

// Readalong is a book's read-along map on the wire (Book.readalong: GET
// /libraries/{id}/item and the console's book page only, never a list row;
// omitted when the book has no ebook or no valid map). A ready map carries
// its text coverage and when it was made; a stale one only why.
type Readalong struct {
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	Coverage    *float64 `json:"coverage,omitempty"`
	GeneratedAt string   `json:"generated_at,omitempty"`
}

// ReadalongState is the state of a map with head for a book whose ebook's
// sha256 is ebookSHA256 and whose audio files (book-relative, BookAudioFiles)
// are files: stale (ebook_changed) when the map was made from another ebook,
// stale (audio_changed) when its audio files aren't the book's (another count,
// another set of paths, or a length off by more than readalongDurationSlack),
// else ready. nil for no map. The caller leaves a book with no ebook out
// before asking (no block at all).
func ReadalongState(head *metadata.ReadalongHead, ebookSHA256 string, files []metadata.ReadalongFile) *Readalong {
	if head == nil {
		return nil
	}
	if !strings.EqualFold(head.Ebook.SHA256, ebookSHA256) {
		return &Readalong{State: ReadalongStale, Reason: ReasonEbookChanged}
	}
	if !sameAudio(head.Audio.Files, files) {
		return &Readalong{State: ReadalongStale, Reason: ReasonAudioChanged}
	}
	coverage := head.Coverage.Text
	return &Readalong{State: ReadalongReady, Coverage: &coverage, GeneratedAt: head.GeneratedAt}
}

// sameAudio reports whether a map's audio files are a book's: as many, the
// same paths as a set (each once), each length within readalongDurationSlack.
func sameAudio(mapped, book []metadata.ReadalongFile) bool {
	if len(mapped) != len(book) {
		return false
	}
	lengths := make(map[string]float64, len(book))
	for _, f := range book {
		lengths[f.Path] = f.Duration
	}
	if len(lengths) != len(book) {
		return false
	}
	seen := make(map[string]bool, len(mapped))
	for _, f := range mapped {
		d, ok := lengths[f.Path]
		if !ok || seen[f.Path] || !(math.Abs(d-f.Duration) <= readalongDurationSlack) {
			return false
		}
		seen[f.Path] = true
	}
	return true
}

// BookAudioFiles is a book's audio files as a read-along map names them: each
// path relative to the book (a folder book's to its folder, a single-file
// book's its base name), with its indexed length. files are the book's files
// (library-relative paths); a book with none (a single-file book) is its own
// one file of length duration.
func BookAudioFiles(bookPath string, folder bool, duration float64, files []BookFile) []metadata.ReadalongFile {
	if len(files) == 0 {
		return []metadata.ReadalongFile{{Path: path.Base(bookPath), Duration: duration}}
	}
	out := make([]metadata.ReadalongFile, len(files))
	for i, f := range files {
		rel := f.RelPath
		if !folder {
			rel = path.Base(rel)
		} else if bookPath != "" && bookPath != "." {
			rel = strings.TrimPrefix(rel, bookPath+"/")
		}
		out[i] = metadata.ReadalongFile{Path: rel, Duration: f.Duration}
	}
	return out
}

// ReadalongIndexed is what the index holds about a book's read-along map: its
// checked head (nil for none or one that failed its checks), the ebook's
// stored sha256, and whether that digest is current (computed for the ebook
// the book has now: ebook_hashed_sig is its ebook_sig).
type ReadalongIndexed struct {
	Head          *metadata.ReadalongHead
	EbookSHA256   string
	DigestCurrent bool
}

// BookReadalongAt is the read-along map the index holds for the book at
// bookPath (its own path). ErrNotFound when no indexed book is there.
func (c *Catalog) BookReadalongAt(ctx context.Context, libraryID int64, bookPath string) (ReadalongIndexed, error) {
	var raw string
	var out ReadalongIndexed
	err := c.db.QueryRowContext(ctx,
		`SELECT COALESCE(a.readalong, ''), b.ebook_sha256, b.ebook_sig != '' AND b.ebook_hashed_sig = b.ebook_sig
		   FROM books b LEFT JOIN book_attachments a ON a.book_id = b.id
		  WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, bookPath).Scan(&raw, &out.EbookSHA256, &out.DigestCurrent)
	if errors.Is(err, sql.ErrNoRows) {
		return ReadalongIndexed{}, ErrNotFound
	}
	if err != nil {
		return ReadalongIndexed{}, err
	}
	out.Head = decodeAttachment[metadata.ReadalongHead](raw)
	return out, nil
}

// EbookDue is a book whose ebook the scan should hash: it has a read-along
// map, and no digest for the ebook it has now.
type EbookDue struct {
	BookPath  string
	EbookPath string
	IsFolder  bool
}

// ReadalongEbooksDue lists the books of a library with a stored read-along map
// and an ebook whose digest isn't current (never hashed, or hashed for a file
// that changed since: ebook_hashed_sig != ebook_sig), narrowed to the book at
// bookPath when it isn't "". Only these are hashed: hashing every ebook of a
// library would read it whole over a network share. octet_length reads the
// column's length from the record header, never its value.
func (c *Catalog) ReadalongEbooksDue(ctx context.Context, libraryID int64, bookPath string) ([]EbookDue, error) {
	q := `SELECT b.rel_path, b.ebook_path, b.is_folder
	        FROM book_attachments a JOIN books b ON b.id = a.book_id
	       WHERE b.library_id = ? AND octet_length(a.readalong) > 0 AND b.ebook_path != ''
	         AND b.ebook_hashed_sig != b.ebook_sig`
	args := []any{libraryID}
	if bookPath != "" {
		q += ` AND b.rel_path = ?`
		args = append(args, bookPath)
	}
	return queryRows(ctx, c.db, func(rows *sql.Rows, d *EbookDue) error {
		return rows.Scan(&d.BookPath, &d.EbookPath, &d.IsFolder)
	}, q, args...)
}

// SetEbookDigest records the sha256 of the book at bookPath's ebook, computed
// for the file whose signature (EbookSig) is sig, only while the row's ebook is
// still that file (ebook_sig = sig): a digest of a file the index has moved on
// from is never recorded as current.
func (c *Catalog) SetEbookDigest(ctx context.Context, libraryID int64, bookPath, sig, sha256 string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE books SET ebook_sha256 = ?, ebook_hashed_sig = ? WHERE library_id = ? AND rel_path = ? AND ebook_sig = ?`,
		sha256, sig, libraryID, bookPath, sig)
	return err
}
