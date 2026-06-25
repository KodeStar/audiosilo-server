package catalog

import (
	"context"
	"errors"
)

// ErrInvalidOverrideMode marks a folder override whose mode is outside the
// {book, collection} allowlist. The transport layer maps it to 400 — the single
// source of truth for the allowlist lives here, not duplicated in the handler.
var ErrInvalidOverrideMode = errors.New(`folder override mode must be "book" or "collection"`)

// Folder detection override modes. The scanner auto-classifies each folder as a
// single book or a collection of separate books; an admin can override a folder
// the heuristic gets wrong. Overrides are durable, path-keyed config (no FK to
// the rebuildable books index) so they survive a re-scan/rebuild.
const (
	// OverrideBook forces the folder to be one (possibly multi-file) book.
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

// SetFolderOverride upserts a detection override for a folder.
func (c *Catalog) SetFolderOverride(ctx context.Context, libraryID int64, path, mode string) error {
	if mode != OverrideBook && mode != OverrideCollection {
		return ErrInvalidOverrideMode
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO folder_overrides(library_id, path, mode, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		libraryID, path, mode, c.ts())
	return err
}

// DeleteFolderOverride removes a folder's override, reverting it to auto-detection.
func (c *Catalog) DeleteFolderOverride(ctx context.Context, libraryID int64, path string) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM folder_overrides WHERE library_id = ? AND path = ?`, libraryID, path)
	return err
}
