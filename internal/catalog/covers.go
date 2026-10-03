package catalog

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// MaxCoverBytes caps a custom cover upload. A cover is shown at most a few hundred
// pixels wide; 5 MiB is generous for that and keeps a row from bloating the DB.
const MaxCoverBytes = 5 << 20

// Why a cover upload is refused.
var (
	ErrUnsupportedImage = errors.New("a cover must be a JPEG, PNG or WebP image")
	ErrCoverTooLarge    = errors.New("the cover image is too large")
)

// coverTypes are the image types a custom cover may be. The type is sniffed from
// the bytes, never taken from the uploader, because it is served back as the
// cover's Content-Type (so no SVG, no HTML).
var coverTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// SetCover stores a custom cover for an indexed book, replacing any earlier one.
// ErrNotFound when no book is indexed at the path; ErrUnsupportedImage or
// ErrCoverTooLarge when the image is refused.
func (c *Catalog) SetCover(ctx context.Context, libraryID int64, path string, data []byte, userID int64) error {
	if len(data) > MaxCoverBytes {
		return ErrCoverTooLarge
	}
	mime := http.DetectContentType(data)
	if !coverTypes[mime] {
		return ErrUnsupportedImage
	}
	path = CleanRelPath(path)
	if _, err := bookIDByPath(ctx, c.db, libraryID, path); err != nil {
		return err
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO book_covers(library_id, path, mime, data, updated_by, updated_at) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     mime = excluded.mime, data = excluded.data,
		     updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		libraryID, path, mime, data, nullableID(userID), c.ts())
	return err
}

// CustomCover is an uploaded cover image. Data is nil from CoverInfo.
type CustomCover struct {
	Data      []byte
	MIME      string
	UpdatedAt string // RFC3339Nano
}

// CoverInfo returns a book path's custom cover without its image bytes, so a
// conditional request can be answered without reading the blob. ErrNotFound when
// the path has none.
func (c *Catalog) CoverInfo(ctx context.Context, libraryID int64, path string) (*CustomCover, error) {
	var cv CustomCover
	err := c.db.QueryRowContext(ctx,
		`SELECT mime, updated_at FROM book_covers WHERE library_id = ? AND path = ?`,
		libraryID, CleanRelPath(path)).Scan(&cv.MIME, &cv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cv, nil
}

// Cover returns a book path's custom cover with its image, or ErrNotFound.
func (c *Catalog) Cover(ctx context.Context, libraryID int64, path string) (*CustomCover, error) {
	var cv CustomCover
	err := c.db.QueryRowContext(ctx,
		`SELECT data, mime, updated_at FROM book_covers WHERE library_id = ? AND path = ?`,
		libraryID, CleanRelPath(path)).Scan(&cv.Data, &cv.MIME, &cv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cv, nil
}

// DeleteCover removes a book path's custom cover (back to its own art). Removing a
// cover that isn't there is not an error.
func (c *Catalog) DeleteCover(ctx context.Context, libraryID int64, path string) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM book_covers WHERE library_id = ? AND path = ?`, libraryID, CleanRelPath(path))
	return err
}
