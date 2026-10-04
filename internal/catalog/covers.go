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

// A custom cover is durable path-keyed state, so it outlives its book being pruned
// (and returns with it), but it is served only while a book is indexed at the path:
// a vanished book has no cover, custom or not.
const coverIndexedJoin = ` FROM book_covers cv
	JOIN books b ON b.library_id = cv.library_id AND b.rel_path = cv.path
	WHERE cv.library_id = ? AND cv.path = ?`

// CoverInfo returns a book path's custom cover without its image bytes, so a
// conditional request can be answered without reading the blob. ErrNotFound when
// the path has none, or no book is indexed there.
func (c *Catalog) CoverInfo(ctx context.Context, libraryID int64, path string) (*CustomCover, error) {
	var cv CustomCover
	err := c.db.QueryRowContext(ctx, `SELECT cv.mime, cv.updated_at`+coverIndexedJoin,
		libraryID, CleanRelPath(path)).Scan(&cv.MIME, &cv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cv, nil
}

// Cover returns a book path's custom cover with its image, or ErrNotFound (as
// CoverInfo).
func (c *Catalog) Cover(ctx context.Context, libraryID int64, path string) (*CustomCover, error) {
	var cv CustomCover
	err := c.db.QueryRowContext(ctx, `SELECT cv.data, cv.mime, cv.updated_at`+coverIndexedJoin,
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

// CoverSource is where an indexed book's cover is read from, without reading it:
// its custom cover's stamp, else its sidecar image, else the art embedded in its
// first audio file (the order GET /libraries/{id}/cover serves them in). Paths
// are library-relative.
type CoverSource struct {
	CustomAt  string // updated_at of the custom cover; "" = none
	CoverPath string // the sidecar image the scanner recorded; "" = none
	AudioPath string // the audio file whose embedded art is the fallback
}

// ArtFiles is the book's own art (no custom cover) as a CoverSource: its sidecar
// image and the audio file embedded art is read from.
func (b *Book) ArtFiles() CoverSource {
	audio := b.RelPath
	if b.IsFolder && len(b.Files) > 0 {
		audio = b.Files[0].RelPath
	}
	return CoverSource{CoverPath: b.CoverPath, AudioPath: audio}
}

// CoverSources resolves the cover source of each book indexed at one of paths in
// a library, in one query (a page of cover thumbnails). A path with no indexed
// book is absent from the map. Paths match exactly, as stored.
func (c *Catalog) CoverSources(ctx context.Context, libraryID int64, paths []string) (map[string]CoverSource, error) {
	out := make(map[string]CoverSource, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, libraryID)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT b.rel_path, COALESCE(cv.updated_at, ''), b.cover_path,
		       CASE WHEN b.is_folder THEN COALESCE(
		         (SELECT bf.rel_path FROM book_files bf WHERE bf.book_id = b.id ORDER BY bf.seq LIMIT 1),
		         b.rel_path) ELSE b.rel_path END
		  FROM books b
		  LEFT JOIN book_covers cv ON cv.library_id = b.library_id AND cv.path = b.rel_path
		 WHERE b.library_id = ? AND b.rel_path IN (`+placeholders(len(paths))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var src CoverSource
		if err := rows.Scan(&path, &src.CustomAt, &src.CoverPath, &src.AudioPath); err != nil {
			return nil, err
		}
		out[path] = src
	}
	return out, rows.Err()
}
