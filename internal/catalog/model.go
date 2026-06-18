// Package catalog stores and queries the audiobook index: libraries, per-user
// access grants and books (with files and chapters). It keeps the FTS5 search
// table in sync on every book upsert/delete. The index is derived from a
// filesystem scan and can be rebuilt at any time.
package catalog

import (
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// Views.
const (
	ViewFilesystem = "filesystem"
	ViewComputed   = "computed"
	ViewHybrid     = "hybrid"
)

// Library is a configured root of audiobooks.
type Library struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Root        string `json:"root"`
	Layout      string `json:"layout"`
	DefaultView string `json:"default_view"`
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
	Size        int64              `json:"size"`
	MTime       int64              `json:"-"`
	AddedAt     string             `json:"added_at,omitempty"` // RFC3339; filesystem birth time (scanner)
	ContentHash string             `json:"-"`
	Files       []BookFile         `json:"files,omitempty"`
	Chapters    []metadata.Chapter `json:"chapters,omitempty"`
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

func (c *Catalog) ts() string { return c.now().UTC().Format(time.RFC3339) }
