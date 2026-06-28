package catalog

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// ErrNotFound is returned when a library or book does not exist.
var ErrNotFound = errors.New("not found")

// ErrInvalidCursor marks a malformed pagination cursor so the transport layer
// can map it to 400 (client error) and distinguish it from an internal failure.
var ErrInvalidCursor = errors.New("invalid cursor")

// ErrNameTaken marks a create or rename that collides with an existing unique
// name (a library or share). The transport layer maps it to 409 Conflict so a
// recoverable client mistake isn't reported as an internal server error.
var ErrNameTaken = errors.New("name already taken")

// CreateLibrary inserts a library and returns it with its assigned ID.
func (c *Catalog) CreateLibrary(ctx context.Context, lib Library) (*Library, error) {
	if lib.DefaultView == "" {
		lib.DefaultView = ViewHybrid
	}
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO libraries(name, root, default_view, created_at)
		 VALUES(?,?,?,?)`, lib.Name, lib.Root, lib.DefaultView, c.ts())
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	lib.ID, _ = res.LastInsertId()
	return &lib, nil
}

// UpsertLibraryByName creates or updates a library keyed by name. Used to sync
// libraries declared in config into the database.
func (c *Catalog) UpsertLibraryByName(ctx context.Context, lib Library) (*Library, error) {
	existing, err := c.GetLibraryByName(ctx, lib.Name)
	if errors.Is(err, ErrNotFound) {
		return c.CreateLibrary(ctx, lib)
	}
	if err != nil {
		return nil, err
	}
	if lib.DefaultView == "" {
		lib.DefaultView = existing.DefaultView
	}
	_, err = c.db.ExecContext(ctx,
		`UPDATE libraries SET root = ?, default_view = ? WHERE id = ?`,
		lib.Root, lib.DefaultView, existing.ID)
	if err != nil {
		return nil, err
	}
	lib.ID = existing.ID
	return &lib, nil
}

// UpdateLibrary updates a library's mutable fields and returns the result.
// Changing the root makes the index stale, so callers should trigger a rescan
// afterward.
func (c *Catalog) UpdateLibrary(ctx context.Context, id int64, in Library) (*Library, error) {
	existing, err := c.GetLibrary(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		existing.Name = in.Name
	}
	if in.Root != "" {
		existing.Root = in.Root
	}
	if in.DefaultView != "" {
		existing.DefaultView = in.DefaultView
	}
	if _, err := c.db.ExecContext(ctx,
		`UPDATE libraries SET name = ?, root = ?, default_view = ? WHERE id = ?`,
		existing.Name, existing.Root, existing.DefaultView, id); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	return existing, nil
}

// DeleteLibrary removes a library and everything indexed under it. Books cascade
// via foreign keys; their FTS rows (a standalone table, not FK-cascaded) are
// removed first via DeleteBooksNotIn with an empty keep set.
func (c *Catalog) DeleteLibrary(ctx context.Context, id int64) error {
	if _, err := c.DeleteBooksNotIn(ctx, id, nil); err != nil {
		return err
	}
	_, err := c.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	return err
}

func scanLibrary(row interface{ Scan(...any) error }) (*Library, error) {
	var l Library
	if err := row.Scan(&l.ID, &l.Name, &l.Root, &l.DefaultView, &l.SortOrder); err != nil {
		return nil, err
	}
	return &l, nil
}

// GetLibrary returns a library by ID.
func (c *Catalog) GetLibrary(ctx context.Context, id int64) (*Library, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT id, name, root, default_view, sort_order FROM libraries WHERE id = ?`, id)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// GetLibraryByName returns a library by name.
func (c *Catalog) GetLibraryByName(ctx context.Context, name string) (*Library, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT id, name, root, default_view, sort_order FROM libraries WHERE name = ?`, name)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// ListLibraries returns all libraries in display order (sort_order, then name).
func (c *Catalog) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, root, default_view, sort_order FROM libraries ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLibraries(rows)
}

// ReorderLibraries sets each library's sort_order to its position in ids (0-based)
// in one transaction; libraries not listed keep their current order. This is the
// admin "reorder libraries" control, and the order it sets is the tiebreaker used
// when de-duplicating identical copies of a book across libraries.
func (c *Catalog) ReorderLibraries(ctx context.Context, ids []int64) error {
	return c.db.WithTx(ctx, "ReorderLibraries", func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE libraries SET sort_order = ? WHERE id = ?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func collectLibraries(rows *sql.Rows) ([]Library, error) {
	var out []Library
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}
