// Package catalog stores and queries the audiobook index: libraries, per-user
// access grants and books (with files and chapters). It keeps the FTS5 search
// table in sync on every book upsert/delete. The index is derived from a
// filesystem scan and can be rebuilt at any time.
package catalog

import (
	"context"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// ViewHybrid is the default library view (filesystem browse + indexed metadata).
const ViewHybrid = "hybrid"

// Library is a configured root of audiobooks. Its shape (single-file books,
// folder-per-book, multi-file parts) is auto-detected per folder by the scanner;
// there is no library-wide layout setting. Folders the detector gets wrong can be
// corrected with a per-folder override (see folder_overrides.go).
type Library struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Root        string `json:"root"`
	DefaultView string `json:"default_view"`
	// SortOrder is the library's display order (lower first). It also breaks ties
	// when de-duplicating identical copies of a book across libraries.
	SortOrder int `json:"sort_order"`
}

// Book is an indexed audiobook (single file or a folder of files).
type Book struct {
	ID          int64              `json:"id"`
	LibraryID   int64              `json:"library_id"`
	RelPath     string             `json:"rel_path"`
	IsFolder    bool               `json:"is_folder"`
	Title       string             `json:"title"`
	Author      string             `json:"author"`
	Series      string             `json:"series"`
	SeriesIndex float64            `json:"series_index"`
	Narrator    string             `json:"narrator"`
	Duration    float64            `json:"duration"`
	ASIN        string             `json:"asin,omitempty"`
	ISBN        string             `json:"isbn,omitempty"`
	CoverPath   string             `json:"-"`
	Format      string             `json:"format"`
	Codec       string             `json:"codec,omitempty"` // audio codec (ffprobe); "" when unknown
	Size        int64              `json:"size"`
	MTime       int64              `json:"-"`
	AddedAt     string             `json:"added_at,omitempty"` // RFC3339; filesystem birth time (scanner)
	ContentHash string             `json:"-"`
	Files       []BookFile         `json:"files,omitempty"`
	Chapters    []metadata.Chapter `json:"chapters,omitempty"`

	// DirectPlayable, when set, reports whether the audio codec plays natively in
	// browsers (so the client knows when to request ?transcode=1). Computed by the
	// API for single-book responses; nil/omitted in list views.
	DirectPlayable *bool `json:"direct_playable,omitempty"`

	// DedupKey groups copies of the same logical book (across libraries, and later
	// across servers) so a client can collapse duplicates. It is a display-grouping
	// HINT, not an identity - never key durable state on it. Set on de-duplicated
	// list responses (search / recent).
	DedupKey string `json:"dedup_key,omitempty"`
	// MultiFile reports whether the book has more than one audio file (a multipart
	// book). Used to rank copies when de-duplicating (a single file beats a
	// multipart copy). Set on de-duplicated list responses; nil/omitted elsewhere.
	MultiFile *bool `json:"multi_file,omitempty"`
	// OtherLocations are the same book's other (non-winning) copies in a
	// de-duplicated list, so a client can show "also on X" and let the user switch.
	OtherLocations []BookLocation `json:"other_locations,omitempty"`
}

// BookLocation points at one copy of a book in a particular library - used to
// list the non-winning copies behind a de-duplicated search/recent result.
type BookLocation struct {
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	Path        string `json:"path"`
	Format      string `json:"format,omitempty"`
	Size        int64  `json:"size,omitempty"`
	MultiFile   bool   `json:"multi_file,omitempty"`
}

// BookFile is one audio file belonging to a multi-file book.
type BookFile struct {
	RelPath  string  `json:"rel_path"`
	Seq      int     `json:"seq"`
	Duration float64 `json:"duration"`
	Format   string  `json:"format"`
	Size     int64   `json:"size"`
}

// Catalog provides indexed reads/writes over the store.
type Catalog struct {
	db  *store.DB
	now func() time.Time
}

// New returns a Catalog. now may be nil to use time.Now.
func New(db *store.DB, now func() time.Time) *Catalog {
	if now == nil {
		now = time.Now
	}
	return &Catalog{db: db, now: now}
}

// Ping verifies the underlying database is reachable for reads (backs GET /healthz).
func (c *Catalog) Ping(ctx context.Context) error { return c.db.Ping(ctx) }

// ts is the server's current time as an RFC3339Nano string. Nanosecond precision
// (rather than whole-second RFC3339) keeps two server-stamped writes in the same
// wall-clock second distinguishable for last-write-wins reconciliation; parsers
// using the plain RFC3339 layout still accept the fractional form.
func (c *Catalog) ts() string { return c.now().UTC().Format(time.RFC3339Nano) }
