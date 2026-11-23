package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path"
)

// ErrInvalidOverrideMode marks a folder override whose mode is outside the
// {book, collection} allowlist. The transport layer maps it to 400 - the single
// source of truth for the allowlist lives here, not duplicated in the handler.
var ErrInvalidOverrideMode = errors.New(`folder override mode must be "book" or "collection"`)

// Folder detection override modes. The scanner auto-classifies each folder as a
// single book or a collection of separate books; an admin can override a folder
// the heuristic gets wrong. Overrides are durable, path-keyed config (no FK to
// the rebuildable books index) so they survive a re-scan/rebuild.
const (
	// OverrideBook forces the folder to be one (possibly multi-file) book; on a
	// folder whose audio is only in disc folders directly in it (CD1, CD2...; at
	// least two, nothing else with audio beneath it) it joins them into one book
	// (library.joinedBook). On any other folder with no audio of its own it does
	// nothing, as before joining existed.
	OverrideBook = "book"
	// OverrideCollection forces each direct audio file to be its own book.
	OverrideCollection = "collection"
)

// FolderOverrides returns a library's explicit per-folder detection overrides,
// keyed by library-relative folder path (slash-separated). The scanner consults
// these before applying its auto heuristic.
func (c *Catalog) FolderOverrides(ctx context.Context, libraryID int64) (map[string]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT path, mode FROM folder_overrides WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, mode string
		if err := rows.Scan(&path, &mode); err != nil {
			return nil, err
		}
		out[path] = mode
	}
	return out, rows.Err()
}

// SetFolderOverride upserts a detection override for a folder. The path is
// canonicalized on write (see CleanRelPath) so it matches the scanner's
// rel_path keys - a non-canonical key would silently never take effect.
func (c *Catalog) SetFolderOverride(ctx context.Context, libraryID int64, path, mode string) error {
	if mode != OverrideBook && mode != OverrideCollection {
		return ErrInvalidOverrideMode
	}
	path = CleanRelPath(path)
	return c.db.WithTx(ctx, "SetFolderOverride", func(tx *sql.Tx) error {
		if mode == OverrideBook && path != "" {
			if err := checkAudibleOverride(ctx, tx, libraryID, path); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO folder_overrides(library_id, path, mode, updated_at) VALUES(?,?,?,?)
			 ON CONFLICT(library_id, path) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
			libraryID, path, mode, c.ts())
		return err
	})
}

// DeleteFolderOverride removes a folder's override, reverting it to auto-detection.
func (c *Catalog) DeleteFolderOverride(ctx context.Context, libraryID int64, path string) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM folder_overrides WHERE library_id = ? AND path = ?`, libraryID, CleanRelPath(path))
	return err
}

// FolderFileLengths returns the indexed lengths of files (a page of a listing,
// at most a few hundred) as parts of the folder book at folder, or of the
// joined book its folder is one disc of.
func (c *Catalog) FolderFileLengths(ctx context.Context, libraryID int64, folder string, files []string) (map[string]float64, error) {
	if len(files) == 0 {
		return map[string]float64{}, nil
	}
	args := make([]any, 0, len(files)+3)
	args = append(args, libraryID, folder, path.Dir(folder))
	for _, f := range files {
		args = append(args, f)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT f.rel_path, f.duration FROM books b
        JOIN book_files f ON f.book_id = b.id WHERE b.library_id = ? AND b.is_folder = 1
        AND b.rel_path IN (?, ?) AND f.rel_path IN (`+placeholders(len(files))+`)
        ORDER BY length(b.rel_path)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lengths := make(map[string]float64)
	for rows.Next() {
		var file string
		var duration float64
		if err := rows.Scan(&file, &duration); err != nil {
			return nil, err
		}
		lengths[file] = duration
	}
	return lengths, rows.Err()
}
