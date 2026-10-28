package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"

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
	if err := checkMetadataSource(&lib); err != nil {
		return nil, err
	}
	if lib.Kind == "" {
		lib.Kind = KindBooks
	}
	if lib.Kind != KindBooks && lib.Kind != KindPodcasts {
		return nil, ErrInvalidKind
	}
	var id int64
	err := c.db.WithTx(ctx, "CreateLibrary", func(tx *sql.Tx) error {
		if _, err := checkLibraryRoot(ctx, tx, lib); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO libraries(name,root,default_view,scan_schedule,ignore_patterns,metadata_source,created_at,kind) VALUES(?,?,?,?,?,?,?,?)`, lib.Name, lib.Root, lib.DefaultView, lib.ScanSchedule, joinPatterns(lib.IgnorePatterns), lib.MetadataSource, c.ts(), lib.Kind)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	lib.ID = id
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
	if lib.Kind != "" && lib.Kind != existing.Kind {
		return nil, ErrKindImmutable
	}
	returnLib, _, err := c.UpdateLibrary(ctx, existing.ID, LibraryPatch{Root: lib.Root, DefaultView: lib.DefaultView})
	if err != nil {
		return nil, err
	}
	lib = *returnLib
	return &lib, nil
}

// LibraryPatch is an edit to a library: an empty string keeps a text field, a nil
// pointer keeps a setting. The scan settings are validated by the caller
// (library.ValidatePatch), the metadata source here (checkMetadataSource).
type LibraryPatch struct {
	Name, Root, DefaultView string
	Kind                    *string
	ScanSchedule            *string
	IgnorePatterns          *[]string
	MetadataSource          *string
}

// Apply writes the patch's fields onto l (a zero Library for a new one).
func (p LibraryPatch) Apply(l *Library) {
	if p.Kind != nil {
		l.Kind = *p.Kind
	}
	if p.Name != "" {
		l.Name = p.Name
	}
	if p.Root != "" {
		l.Root = p.Root
	}
	if p.DefaultView != "" {
		l.DefaultView = p.DefaultView
	}
	if p.ScanSchedule != nil {
		l.ScanSchedule = *p.ScanSchedule
	}
	if p.IgnorePatterns != nil {
		l.IgnorePatterns = *p.IgnorePatterns
	}
	if p.MetadataSource != nil {
		l.MetadataSource = *p.MetadataSource
	}
}

// UpdateLibrary applies an edit and returns the result, and whether it made the
// index stale (a new root or new ignore rules), in which case the caller should
// rescan. A new metadata source is no reason to: the library's books are
// re-resolved from what the scan stored, in the edit's own transaction, so no
// reader sees the new setting with the old values. Whether the source changed is
// read inside that transaction, not from the earlier read: an edit that raced this
// one may have switched it since.
func (c *Catalog) UpdateLibrary(ctx context.Context, id int64, in LibraryPatch) (*Library, bool, error) {
	existing, err := c.GetLibrary(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if in.Kind != nil && *in.Kind != existing.Kind {
		return nil, false, ErrKindImmutable
	}
	before := *existing
	in.Apply(existing)
	if err := checkMetadataSource(existing); err != nil {
		return nil, false, err
	}
	resolved := false // the books were re-resolved (their identifiers may differ)
	err = c.db.WithTx(ctx, "UpdateLibrary", func(tx *sql.Tx) error {
		if _, err := checkLibraryRoot(ctx, tx, *existing); err != nil {
			return err
		}
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT metadata_source FROM libraries WHERE id = ?`, id).
			Scan(&stored); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE libraries SET name = ?, root = ?, default_view = ?, scan_schedule = ?, ignore_patterns = ?,
			     metadata_source = ?
			  WHERE id = ?`,
			existing.Name, existing.Root, existing.DefaultView, existing.ScanSchedule,
			joinPatterns(existing.IgnorePatterns), existing.MetadataSource, id); err != nil {
			return err
		}
		// A podcast library's episodes come from feed.json, never from tags or
		// the path, and have no scanned snapshot: re-resolving them would blank
		// what IndexPodcastShow wrote (their descriptions among it).
		if existing.MetadataSource == stored || existing.Kind == KindPodcasts {
			return nil
		}
		resolved = true
		return refreshLibrary(ctx, tx, id)
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, false, ErrNameTaken
		}
		return nil, false, err
	}
	if resolved {
		c.changed()
	}
	stale := existing.Root != before.Root || !slices.Equal(existing.IgnorePatterns, before.IgnorePatterns)
	return existing, stale, nil
}

// refreshLibrary re-resolves the effective metadata of every book in a library
// (refreshEffective), as a change to how it is resolved needs.
func refreshLibrary(ctx context.Context, tx *sql.Tx, libraryID int64) error {
	ids, err := queryRows(ctx, tx, func(r *sql.Rows, id *int64) error { return r.Scan(id) },
		`SELECT id FROM books WHERE library_id = ?`, libraryID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshEffective(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// joinPatterns and splitPatterns store an ignore list as one pattern per line.
func joinPatterns(p []string) string { return strings.Join(p, "\n") }

func splitPatterns(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// DeleteLibrary removes a library and everything indexed under it. Books cascade
// via foreign keys; their FTS rows (a standalone table, not FK-cascaded) are
// removed first via DeleteBooksNotIn with an empty keep set.
func (c *Catalog) DeleteLibrary(ctx context.Context, id int64) error {
	if _, err := c.DeleteBooksNotIn(ctx, id, nil); err != nil {
		return err
	}
	// The library's whole-library grant shares grant nothing once it's gone
	// (their rule cascades away); drop them rather than leave empty shares. One
	// that also holds another library's rules still grants those, so it stays.
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM shares WHERE whole_library_id = ?
		    AND NOT EXISTS (SELECT 1 FROM share_paths sp
		                     WHERE sp.share_id = shares.id AND sp.library_id != ?)`, id, id); err != nil {
		return err
	}
	_, err := c.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	return err
}

// libraryCols is what scanLibrary reads.
const libraryCols = `id, name, root, default_view, sort_order, scan_schedule, ignore_patterns, metadata_source, kind`

func scanLibrary(row interface{ Scan(...any) error }) (*Library, error) {
	var l Library
	var patterns string
	if err := row.Scan(&l.ID, &l.Name, &l.Root, &l.DefaultView, &l.SortOrder, &l.ScanSchedule, &patterns,
		&l.MetadataSource, &l.Kind); err != nil {
		return nil, err
	}
	l.IgnorePatterns = splitPatterns(patterns)
	return &l, nil
}

// GetLibrary returns a library by ID, from libraryCache while nothing has been
// written since it was read.
func (c *Catalog) GetLibrary(ctx context.Context, id int64) (*Library, error) {
	gen, cacheable := c.Generation() // before the read (libraryCache)
	if cacheable {
		if l, ok := c.libs.Get(gen, id); ok {
			return copyLibrary(l), nil
		}
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+libraryCols+` FROM libraries WHERE id = ?`, id)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err == nil && cacheable {
		c.libs.Put(gen, id, *copyLibrary(*l))
	}
	return l, err
}

// GetLibraryByName returns a library by name.
func (c *Catalog) GetLibraryByName(ctx context.Context, name string) (*Library, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT `+libraryCols+` FROM libraries WHERE name = ?`, name)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// ListLibraries returns all libraries in display order (sort_order, then name).
func (c *Catalog) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+libraryCols+` FROM libraries ORDER BY sort_order, name`)
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

const (
	KindBooks    = "books"
	KindPodcasts = "podcasts"
)

// inBookLibrary keeps a books row (alias b) of a books-kind library only:
// podcast episodes are out of every cross-library book aggregate and the admin
// book tools. It stays a correlated EXISTS: an IN list lets the planner lead
// with the library index and slows the Health counts (TestSeriesKeyScale).
// bookLibraryIDs is the rule for a query over books unaliased.
const (
	inBookLibrary  = `EXISTS(SELECT 1 FROM libraries pl WHERE pl.id=b.library_id AND pl.kind='` + KindBooks + `')`
	bookLibraryIDs = `library_id IN (SELECT id FROM libraries WHERE kind='` + KindBooks + `')`
)

var ErrInvalidKind = errors.New("invalid library kind")
var ErrKindImmutable = errors.New("library kind is immutable")
var ErrRootOverlaps = errors.New("podcast library roots must not overlap other libraries")

// checkLibraryRoot runs with the writer held (q a writer transaction) so
// simultaneous creates cannot admit overlapping roots; CheckLibraryRoot reads it
// without. Paths are cleaned and made absolute on both sides. sameRoot is the
// name of another library at exactly this root (first in the console's order),
// which is allowed between two books libraries.
func checkLibraryRoot(ctx context.Context, q rowQuerier, lib Library) (sameRoot string, err error) {
	root, err := filepath.Abs(lib.Root)
	if err != nil {
		return "", err
	}
	rows, err := q.QueryContext(ctx, `SELECT name,root,kind FROM libraries WHERE id != ? ORDER BY sort_order, name`, lib.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, other, kind string
		if err := rows.Scan(&name, &other, &kind); err != nil {
			return "", err
		}
		other, err = filepath.Abs(other)
		if err != nil {
			return "", err
		}
		if (lib.Kind == KindPodcasts || kind == KindPodcasts) && pathOverlaps(root, other) {
			return "", ErrRootOverlaps
		}
		if other == root && sameRoot == "" {
			sameRoot = name
		}
	}
	return sameRoot, rows.Err()
}
func pathOverlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator)) || a == string(filepath.Separator) || b == string(filepath.Separator)
}

// ListBookLibraries is for cross-library audiobook tools; podcasts have their
// own scanner and are never candidates for book matching or enrichment.
func (c *Catalog) ListBookLibraries(ctx context.Context) ([]Library, error) {
	libs, err := c.ListLibraries(ctx)
	return slices.DeleteFunc(libs, func(l Library) bool { return l.Kind != KindBooks }), err
}

var ErrNotBookLibrary = errors.New("not a books library")
