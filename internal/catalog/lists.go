package catalog

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Ordered lists of books: the up-next queue (up_next, one per user) and a
// collection's items (collection_items, one per collection). Both are durable,
// path-keyed user state with the same semantics, implemented once here
// (orderedList): an entry is a book's (library_id, rel_path); the stored order is
// the position column; a read returns only the entries the READER's current
// access allows (the rows themselves are kept, as for favourites) with the book
// attached when one is indexed at the path.

// ListItem is one entry of an ordered list (an up-next entry, a collection item):
// the book's path identity, when it was added, and the book itself (the list
// shape, as /libraries/{id}/books rows) when one is indexed at the path.
type ListItem struct {
	Ref
	AddedAt string `json:"added_at"`
	Book    *Book  `json:"book,omitempty"`
}

var (
	// ErrListFull is an add to a list already holding its maximum.
	ErrListFull = errors.New("list is full")
	// ErrTooManyItems is a whole-list replace longer than the list's maximum.
	ErrTooManyItems = errors.New("too many items")
)

// orderedList is one table of ordered book refs, each owned by a key (a user id
// for up_next, a collection id for collection_items). The statements are spelled
// out per table rather than built from a table name: concatenating SQL inside a
// transaction trips gosec G202 (see carryListeningState), and only values are
// bound parameters anyway.
type orderedList struct {
	max int
	// load reads an owner's rows in stored order: library_id, rel_path, added_at,
	// position. One parameter: the owner.
	load string
	// visible is load's SELECT up to "WHERE <owner> = ? AND ", for a scope filter
	// (scopesFilterSQL) and then orderBy appended.
	visible string
	// insert: owner, library_id, rel_path, position, added_at.
	insert string
	// setPos: position, owner, library_id, rel_path.
	setPos string
	// remove: owner, library_id, rel_path.
	remove string
}

// orderBy is the stored order of an ordered list's rows: position, ties (a carry
// can leave equal positions only in a database written by hand) by path.
const orderBy = ` ORDER BY position, library_id, rel_path`

// listRow is a stored entry with its stored position.
type listRow struct {
	Ref
	AddedAt string
	pos     int64
}

// rows reads an owner's entries in stored order, inside tx.
func (l orderedList) rows(ctx context.Context, tx *sql.Tx, owner int64) ([]listRow, error) {
	return queryRows(ctx, tx, func(rows *sql.Rows, r *listRow) error {
		return rows.Scan(&r.LibraryID, &r.Path, &r.AddedAt, &r.pos)
	}, l.load, owner)
}

// write stores next as the owner's whole list (positions 0..n-1), given the rows
// stored now (old): rows no longer listed are deleted, new ones inserted, and
// only rows whose position moved are updated. It reports whether anything changed.
func (l orderedList) write(ctx context.Context, tx *sql.Tx, owner int64, old, next []listRow) (bool, error) {
	stored := make(map[Ref]listRow, len(old))
	for _, r := range old {
		stored[r.Ref] = r
	}
	keep := make(map[Ref]bool, len(next))
	for _, r := range next {
		keep[r.Ref] = true
	}
	changed := false
	for _, r := range old {
		if keep[r.Ref] {
			continue
		}
		if _, err := tx.ExecContext(ctx, l.remove, owner, r.LibraryID, r.Path); err != nil {
			return false, err
		}
		changed = true
	}
	for i, r := range next {
		cur, ok := stored[r.Ref]
		switch {
		case !ok:
			if _, err := tx.ExecContext(ctx, l.insert, owner, r.LibraryID, r.Path, i, r.AddedAt); err != nil {
				return false, err
			}
			changed = true
		case cur.pos != int64(i):
			if _, err := tx.ExecContext(ctx, l.setPos, i, owner, r.LibraryID, r.Path); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// add puts ref on the owner's list at position (0-based in the stored order; nil,
// or past the end, = the end; a negative one counts as 0), inside tx. A ref already
// listed moves to position when one is given and otherwise stays where it is (an
// idempotent add). A new ref on a full list is ErrListFull. It reports whether
// anything changed.
func (l orderedList) add(ctx context.Context, tx *sql.Tx, owner int64, ref Ref, position *int, now string) (bool, error) {
	rows, err := l.rows(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	next := slices.Clone(rows)
	entry := listRow{Ref: ref, AddedAt: now}
	if i := slices.IndexFunc(next, func(r listRow) bool { return r.Ref == ref }); i >= 0 {
		if position == nil {
			return false, nil
		}
		entry = next[i]
		next = slices.Delete(next, i, i+1)
	} else if len(next) >= l.max {
		return false, ErrListFull
	}
	at := len(next)
	if position != nil {
		at = min(max(*position, 0), len(next))
	}
	next = slices.Insert(next, at, entry)
	return l.write(ctx, tx, owner, rows, next)
}

// replace makes refs (already deduplicated and checked, see listableRefs) the
// owner's whole list in that order, inside tx: an entry already listed keeps its
// added_at, a new one is added now. More than the list's maximum is
// ErrTooManyItems. It reports whether anything changed.
func (l orderedList) replace(ctx context.Context, tx *sql.Tx, owner int64, refs []Ref, now string) (bool, error) {
	if len(refs) > l.max {
		return false, ErrTooManyItems
	}
	rows, err := l.rows(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	added := make(map[Ref]string, len(rows))
	for _, r := range rows {
		added[r.Ref] = r.AddedAt
	}
	next := make([]listRow, len(refs))
	for i, ref := range refs {
		at, ok := added[ref]
		if !ok {
			at = now
		}
		next[i] = listRow{Ref: ref, AddedAt: at}
	}
	return l.write(ctx, tx, owner, rows, next)
}

// drop removes ref from the owner's list (idempotent), inside tx. The positions
// after it keep their order; the next write renumbers them. It reports whether a
// row was removed.
func (l orderedList) drop(ctx context.Context, tx *sql.Tx, owner int64, ref Ref) (bool, error) {
	res, err := tx.ExecContext(ctx, l.remove, owner, ref.LibraryID, ref.Path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// visibleItems reads an owner's entries the reader can reach now (scopes, the
// reader's UserScopes; none = nothing), in stored order, each with its book
// attached when one is indexed there.
func (c *Catalog) visibleItems(ctx context.Context, l orderedList, owner int64, scopes []Scope) ([]ListItem, error) {
	filter, fargs := scopesFilterSQL("library_id", "rel_path", scopes)
	items, err := queryRows(ctx, c.db, func(rows *sql.Rows, it *ListItem) error {
		var pos int64
		return rows.Scan(&it.LibraryID, &it.Path, &it.AddedAt, &pos)
	}, l.visible+filter+orderBy, append([]any{owner}, fargs...)...)
	if err != nil {
		return nil, err
	}
	return items, c.attachBooks(ctx, items)
}

// attachBooks sets each item's Book from the index (BooksByPaths, one chunked read
// per library), leaving it nil where no book is indexed at the path.
func (c *Catalog) attachBooks(ctx context.Context, items []ListItem) error {
	byLib := map[int64][]string{}
	for _, it := range items {
		byLib[it.LibraryID] = append(byLib[it.LibraryID], it.Path)
	}
	books := make(map[Ref]Book, len(items))
	for libID, paths := range byLib {
		found, err := c.BooksByPaths(ctx, libID, paths)
		if err != nil {
			return err
		}
		for p, b := range found {
			books[Ref{LibraryID: libID, Path: p}] = b
		}
	}
	for i := range items {
		if b, ok := books[items[i].Ref]; ok {
			items[i].Book = &b
		}
	}
	return nil
}

// listableRefs is the skip rule of a whole-list replace: refs in order, each
// path cleaned (CleanRelPath), duplicates collapsed (the first wins), keeping only
// those that name exactly an indexed book (no part path, no indexing on demand)
// the caller's current access allows (scopes). Nothing skipped is an error.
func (c *Catalog) listableRefs(ctx context.Context, refs []Ref, scopes []Scope) ([]Ref, error) {
	byLib := make(map[int64]Scope, len(scopes))
	for _, s := range scopes {
		byLib[s.LibraryID] = s
	}
	seen := make(map[Ref]bool, len(refs))
	var cands []Ref
	paths := map[int64][]string{}
	for _, r := range refs {
		r.Path = CleanRelPath(r.Path)
		s, ok := byLib[r.LibraryID]
		if r.Path == "" || seen[r] || !ok || !s.Allows(r.Path) {
			continue
		}
		seen[r] = true
		cands = append(cands, r)
		paths[r.LibraryID] = append(paths[r.LibraryID], r.Path)
	}
	indexed := map[Ref]bool{}
	for libID, ps := range paths {
		found, err := c.BooksByPaths(ctx, libID, ps)
		if err != nil {
			return nil, err
		}
		for p := range found {
			indexed[Ref{LibraryID: libID, Path: p}] = true
		}
	}
	out := make([]Ref, 0, len(cands))
	for _, r := range cands {
		if indexed[r] {
			out = append(out, r)
		}
	}
	return out, nil
}

// stamp is the time lists and collections record (added_at, created_at,
// updated_at): UTC, fixed width with milliseconds, so the newest-first orders
// compare it as text.
func (c *Catalog) stamp() string { return formatSessionTime(c.now()) }

// carryListsState hands the up-next entries and collection items on one path to
// another, inside tx (a move or one part of a join; see carryListeningState). A
// list that already holds the destination keeps that entry (and its position)
// and drops the moved one, so a collision never fails the carry.
func carryListsState(ctx context.Context, tx *sql.Tx, libraryID int64, from, into string) error {
	for _, stmt := range []string{
		`UPDATE OR IGNORE up_next SET rel_path = ?1 WHERE library_id = ?2 AND rel_path = ?3`,
		`DELETE FROM up_next WHERE library_id = ?2 AND rel_path = ?3`,
		`UPDATE OR IGNORE collection_items SET rel_path = ?1 WHERE library_id = ?2 AND rel_path = ?3`,
		`DELETE FROM collection_items WHERE library_id = ?2 AND rel_path = ?3`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, into, libraryID, from); err != nil {
			return err
		}
	}
	return nil
}
