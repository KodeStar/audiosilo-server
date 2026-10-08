package catalog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
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

// SetCover stores a custom cover for an indexed book, replacing any earlier one,
// and moves the book's cover art (and so its cover_version) to it at once, so a
// client sees the change on its next book fetch rather than when a cached cover
// expires. source says where the image came from: SourceEdited (an upload, also
// for "") or SourceCommunity (a community match's, which ClearCommunityMatches
// removes).
// ErrNotFound when no book is indexed at the path; ErrUnsupportedImage or
// ErrCoverTooLarge when the image is refused.
func (c *Catalog) SetCover(ctx context.Context, libraryID int64, path string, data []byte, userID int64, source string) error {
	source, err := normalizeSource(source)
	if err != nil {
		return err
	}
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
	if err := c.db.WithTx(ctx, "SetCover", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_covers(library_id, path, mime, data, updated_by, updated_at, source) VALUES(?,?,?,?,?,?,?)
			 ON CONFLICT(library_id, path) DO UPDATE SET
			     mime = excluded.mime, data = excluded.data, updated_by = excluded.updated_by,
			     updated_at = excluded.updated_at, source = excluded.source`,
			libraryID, path, mime, data, nullableID(userID), c.ts(), source); err != nil {
			return err
		}
		return refreshCoverArt(ctx, tx, libraryID, path)
	}); err != nil {
		return err
	}
	c.changed()
	return nil
}

// coverArtSQL is a books row's cover art identity from index data alone: "c" and
// its custom cover's updated_at (CustomArtVersion) when it has one, else "f" and
// its mtime, size and sidecar path. Migration 0023 backfills with the same
// expression. It holds until a thumbnail reads the art itself: RecordCoverColors
// then moves it to that art's own version (its file's size and mtime), so the
// cover_version follows a sidecar or embedded image replaced in place, which
// leaves the index (the audio's mtime and size, the sidecar's path) as it was.
const coverArtSQL = `COALESCE(
	(SELECT 'c' || cv.updated_at FROM book_covers cv
	  WHERE cv.library_id = books.library_id AND cv.path = books.rel_path),
	'f' || books.mtime || ' ' || books.size || ' ' || books.cover_path)`

// refreshCoverArt recomputes books.cover_art for the book at path (if one is
// indexed there), after anything coverArtSQL reads has changed.
func refreshCoverArt(ctx context.Context, tx *sql.Tx, libraryID int64, path string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE books SET cover_art = `+coverArtSQL+` WHERE library_id = ? AND rel_path = ?`, libraryID, path)
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

// DeleteCover removes a book path's custom cover (back to its own art, and its
// cover_version back to that art's). Removing a cover that isn't there is not an
// error.
func (c *Catalog) DeleteCover(ctx context.Context, libraryID int64, path string) error {
	path = CleanRelPath(path)
	removed := false
	if err := c.db.WithTx(ctx, "DeleteCover", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM book_covers WHERE library_id = ? AND path = ?`, libraryID, path)
		if err != nil {
			return err
		}
		// Only a removed custom cover changes the art: with none to remove, the
		// book keeps its identity (and its colour), which may already be its
		// image's own version from a thumbnail rather than the index form.
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		removed = true
		return refreshCoverArt(ctx, tx, libraryID, path)
	}); err != nil {
		return err
	}
	if removed {
		c.changed()
	}
	return nil
}

// CoverSource is where an indexed book's cover is read from, without reading it:
// its custom cover's stamp, else its sidecar image, else the art embedded in its
// first audio file (the order GET /libraries/{id}/cover serves them in). Paths
// are library-relative.
type CoverSource struct {
	CustomAt  string // updated_at of the custom cover; "" = none
	CoverPath string // the sidecar image the scanner recorded; "" = none
	AudioPath string // the audio file whose embedded art is the fallback
	// Art is the book's cover art identity (books.cover_art), what a colour read
	// from a thumbnail is recorded against (CoverColorRecord); Colored is whether
	// the book holds a colour for that identity already, or a record that its art
	// has none (a thumbnail also checks the identity is still its art's version).
	Art     string
	Colored bool
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
		SELECT b.rel_path, `+coverSourceCols+coverSourceFrom+`
		 WHERE b.library_id = ? AND b.rel_path IN (`+placeholders(len(paths))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var src CoverSource
		dest, finish := coverSourceDest(&src)
		if err := rows.Scan(append([]any{&path}, dest...)...); err != nil {
			return nil, err
		}
		finish()
		out[path] = src
	}
	return out, rows.Err()
}

// coverSourceCols are a book's CoverSource columns over coverSourceFrom
// (coverSourceDest scans them): the custom cover's stamp, the sidecar, the audio
// file embedded art is read from (a folder book's first), the art identity and
// the stored colour.
const coverSourceCols = `COALESCE(cv.updated_at, ''), b.cover_path,
		       CASE WHEN b.is_folder THEN COALESCE(
		         (SELECT bf.rel_path FROM book_files bf WHERE bf.book_id = b.id ORDER BY bf.seq LIMIT 1),
		         b.rel_path) ELSE b.rel_path END,
		       b.cover_art, b.cover_color`

const coverSourceFrom = `
		  FROM books b
		  LEFT JOIN book_covers cv ON cv.library_id = b.library_id AND cv.path = b.rel_path`

// coverSourceDest returns the scan destinations for coverSourceCols; finish,
// called after the scan, sets Colored.
func coverSourceDest(src *CoverSource) (dest []any, finish func()) {
	var color string
	return []any{&src.CustomAt, &src.CoverPath, &src.AudioPath, &src.Art, &color},
		func() { _, src.Colored = storedCoverColor(src.Art, color) }
}

// CustomArtVersion is the art version of a custom cover stored at stamp (its
// updated_at): what thumbnails of it are cached under, and its cover art identity
// (coverArtSQL). File art is versioned by the reader of the file (its size and
// mtime).
func CustomArtVersion(stamp string) string { return "c" + stamp }

// CoverVersion is the opaque cover_version token for a cover art identity or art
// version: a short hash, so the wire carries no stamp, size or path detail. ""
// for "".
func CoverVersion(art string) string {
	if art == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(art))
	return hex.EncodeToString(sum[:])[:10]
}

// CoverColorRecord is a colour read from a thumbnail of a book's cover: Art is
// the book's cover art identity (CoverSource.Art) when its source was read, and
// Version the version of the art the thumbnail was made of (CustomArtVersion, or
// its file's size and mtime), which becomes the book's identity with the colour.
type CoverColorRecord struct {
	LibraryID int64
	Path      string
	Art       string
	Version   string
	Color     CoverColor
}

// RecordCoverColors stores thumbnails' colours on their books, in one
// transaction, moving each book's cover art identity to the version of the art
// read (so its cover_version moves with an image replaced in place). Each is
// compare-and-set on the identity it was read under: a book whose art has moved
// on since (a custom cover set or removed, a re-index) keeps what it has, so a
// slow thumbnail of old art never lands on new art. A zero Color records that
// the art has no colour to read (no art, or none that decodes), so the background
// pass doesn't read it again until it changes.
func (c *Catalog) RecordCoverColors(ctx context.Context, recs []CoverColorRecord) error {
	if len(recs) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "RecordCoverColors", func(tx *sql.Tx) error {
		for _, r := range recs {
			art := cmp.Or(r.Version, r.Art)
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET cover_art = ?, cover_color = ? WHERE library_id = ? AND rel_path = ? AND cover_art = ?`,
				art, encodeCoverColor(CoverVersion(art), r.Color), r.LibraryID, r.Path, r.Art); err != nil {
				return err
			}
		}
		return nil
	})
}

// CoverColorDue is a book that may have cover art but holds no colour for it
// (CoverColorsDue), with where its cover comes from.
type CoverColorDue struct {
	Ref
	Source CoverSource
	id     int64
}

// CoverColorsDue lists the books after afterID (by the index's own order) that
// may have cover art, a custom cover or art a scan saw or hasn't checked for, but
// hold no colour for their current art, nor a record that it has none: what the
// background colour pass reads thumbnails of. It reads up to limit books and
// returns those due among them, and next, the id to continue after (0 once there
// are no more). The ids stay inside the catalog; a caller only hands next back.
func (c *Catalog) CoverColorsDue(ctx context.Context, afterID int64, limit int) (due []CoverColorDue, next int64, err error) {
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, d *CoverColorDue) error {
		dest, finish := coverSourceDest(&d.Source)
		if err := rows.Scan(append([]any{&d.id, &d.LibraryID, &d.Path}, dest...)...); err != nil {
			return err
		}
		finish()
		return nil
	}, `SELECT b.id, b.library_id, b.rel_path, `+coverSourceCols+coverSourceFrom+`
		 WHERE b.id > ? AND (COALESCE(b.has_cover, 1) = 1 OR cv.path IS NOT NULL)
		 ORDER BY b.id LIMIT ?`, afterID, limit+1)
	if err != nil {
		return nil, 0, err
	}
	rows, next = pageBefore(rows, limit, func(d CoverColorDue) int64 { return d.id })
	return slices.DeleteFunc(rows, func(d CoverColorDue) bool { return d.Source.Colored }), next, nil
}

// encodeCoverColor is a colour as books.cover_color stores it, tagged with the
// cover_version it was read for: "version bg" or "version bg accent on_accent"
// (space-separated), or "version" alone for art read and found to have no colour
// (a zero cc: no art, or none that decodes).
func encodeCoverColor(version string, cc CoverColor) string {
	s := version
	if cc.Bg != "" {
		s += " " + cc.Bg
	}
	if cc.Accent != "" {
		s += " " + cc.Accent + " " + cc.OnAccent
	}
	return s
}

// storedCoverColor reads a book's stored colour (books.cover_color, as
// encodeCoverColor writes it) for its cover art identity (books.cover_art): ok
// when one was read for that art, with the colour, or nil when the art has none;
// not ok when nothing was read for it (a colour read for other art doesn't count).
func storedCoverColor(art, color string) (cc *CoverColor, ok bool) {
	version := CoverVersion(art)
	parts := strings.Fields(color)
	if version == "" || len(parts) == 0 || parts[0] != version {
		return nil, false
	}
	if len(parts) == 1 {
		return nil, true
	}
	cc = &CoverColor{Bg: parts[1]}
	if len(parts) == 4 {
		cc.Accent, cc.OnAccent = parts[2], parts[3]
	}
	return cc, true
}
