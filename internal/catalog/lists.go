package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// ErrListFull is an add to a list already holding its maximum. Each list
	// returns its own error wrapping it (ErrQueueFull, ErrCollectionFull).
	ErrListFull = errors.New("list is full")
	// ErrQueueFull is an add to a full up-next queue (MaxQueue).
	ErrQueueFull = fmt.Errorf("%w: the queue holds at most %d books", ErrListFull, MaxQueue)
	// ErrCollectionFull is an add to a full collection (MaxCollectionItems).
	ErrCollectionFull = fmt.Errorf("%w: a collection holds at most %d books", ErrListFull, MaxCollectionItems)
	// ErrTooManyItems is a whole-list replace longer than the list's maximum.
	ErrTooManyItems = errors.New("too many items")
)

// orderedList is one table of ordered book refs, each owned by a key (a user id
// for up_next, a collection id for collection_items). The statements are spelled
// out per table rather than built from a table name: concatenating SQL inside a
// transaction trips gosec G202 (see carryListeningState), and only values are
// bound parameters anyway.
//
// position orders an owner's rows and need not be dense: a remove leaves a gap,
// an add at an index shifts only the rows from there on, and a replace keeps
// every row whose stored position still fits its new place. A 0-based index (an
// add's position) is a rank in the stored order, hidden rows included.
type orderedList struct {
	max int
	// full is the error of an add to a full list (wraps ErrListFull).
	full error
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
	// shift moves the rows at or after a position one place on: owner, position.
	shift string
	// remove: owner, library_id, rel_path.
	remove string
}

// listTx is the transaction an orderedList reads and writes in (a *sql.Tx).
type listTx interface {
	rowQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
func (l orderedList) rows(ctx context.Context, tx listTx, owner int64) ([]listRow, error) {
	return queryRows(ctx, tx, func(rows *sql.Rows, r *listRow) error {
		return rows.Scan(&r.LibraryID, &r.Path, &r.AddedAt, &r.pos)
	}, l.load, owner)
}

// fits is the length check of a whole-list replace, made once, on the refs as
// given (before de-duplication and the skip rule): more than the list's maximum
// is ErrTooManyItems.
func (l orderedList) fits(refs []Ref) error {
	if len(refs) > l.max {
		return ErrTooManyItems
	}
	return nil
}

// add puts ref on the owner's list at position (0-based in the stored order; nil,
// or past the end, = the end; a negative one counts as 0), inside tx. A ref already
// listed moves to position when one is given and otherwise stays where it is (an
// idempotent add). A new ref on a full list is the list's full error. It reports
// whether anything changed. Whatever the list's length, the write is at most one
// range shift and one insert or update.
func (l orderedList) add(ctx context.Context, tx listTx, owner int64, ref Ref, position *int, now string) (bool, error) {
	rows, err := l.rows(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	cur := slices.IndexFunc(rows, func(r listRow) bool { return r.Ref == ref })
	switch {
	case cur >= 0 && position == nil:
		return false, nil
	case cur < 0 && len(rows) >= l.max:
		return false, l.full
	}
	rest := rows // the other rows, in stored order
	if cur >= 0 {
		rest = slices.Delete(slices.Clone(rows), cur, cur+1)
	}
	at := len(rest)
	if position != nil {
		at = min(max(*position, 0), len(rest))
	}
	if at == cur {
		return false, nil // already there
	}
	var pos int64
	switch {
	case at == len(rest) && at > 0:
		pos = rest[at-1].pos + 1
	case at == len(rest): // an empty list
		pos = 0
	case at > 0 && rest[at-1].pos >= rest[at].pos:
		// Equal positions (a hand-written database) can't take a shift between
		// them: number the whole list afresh.
		entry := listRow{Ref: ref, AddedAt: now}
		if cur >= 0 {
			entry = rows[cur]
		}
		return l.write(ctx, tx, owner, rows, numbered(slices.Insert(slices.Clone(rest), at, entry)))
	default:
		pos = rest[at].pos
		if _, err := tx.ExecContext(ctx, l.shift, owner, pos); err != nil {
			return false, err
		}
	}
	if cur >= 0 {
		_, err = tx.ExecContext(ctx, l.setPos, pos, owner, ref.LibraryID, ref.Path)
	} else {
		_, err = tx.ExecContext(ctx, l.insert, owner, ref.LibraryID, ref.Path, pos, now)
	}
	return err == nil, err
}

// replace makes refs (already deduplicated and checked, see listableRefs and
// fits) the owner's whole list in that order, inside tx: an entry already listed
// keeps its added_at, a new one is added now. It reports whether anything
// changed.
func (l orderedList) replace(ctx context.Context, tx listTx, owner int64, refs []Ref, now string) (bool, error) {
	rows, err := l.rows(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	stored := make(map[Ref]listRow, len(rows))
	for _, r := range rows {
		stored[r.Ref] = r
	}
	next := make([]listRow, len(refs))
	for i, ref := range refs {
		r, ok := stored[ref]
		if !ok {
			r = listRow{Ref: ref, AddedAt: now, pos: -1}
		}
		next[i] = r
	}
	return l.write(ctx, tx, owner, rows, placed(next))
}

// placed gives next (in its new order; a row's pos is its stored position, -1
// for a new one) strictly increasing positions, keeping as many stored ones as
// it cheaply can: either each row keeps its position while it is above the
// previous row's (taking the previous + 1 otherwise), or the list is numbered
// 0..n-1, whichever moves fewer stored rows.
func placed(next []listRow) []listRow {
	kept := make([]listRow, len(next))
	last := int64(-1)
	for i, r := range next {
		if r.pos <= last {
			r.pos = last + 1
		}
		kept[i], last = r, r.pos
	}
	dense := numbered(slices.Clone(next))
	if moves(next, dense) < moves(next, kept) {
		return dense
	}
	return kept
}

// numbered sets rows' positions to 0..n-1, in place, and returns rows.
func numbered(rows []listRow) []listRow {
	for i := range rows {
		rows[i].pos = int64(i)
	}
	return rows
}

// moves counts the rows a placement writes: new ones and moved ones.
func moves(was, now []listRow) int {
	n := 0
	for i := range was {
		if was[i].pos != now[i].pos {
			n++
		}
	}
	return n
}

// write stores next (with its positions) as the owner's whole list, given the
// rows stored now (old): rows no longer listed are deleted, new ones inserted,
// and only rows whose position moved are updated. It reports whether anything
// changed.
func (l orderedList) write(ctx context.Context, tx listTx, owner int64, old, next []listRow) (bool, error) {
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
	for _, r := range next {
		cur, ok := stored[r.Ref]
		switch {
		case !ok:
			if _, err := tx.ExecContext(ctx, l.insert, owner, r.LibraryID, r.Path, r.pos, r.AddedAt); err != nil {
				return false, err
			}
			changed = true
		case cur.pos != r.pos:
			if _, err := tx.ExecContext(ctx, l.setPos, r.pos, owner, r.LibraryID, r.Path); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// drop removes ref from the owner's list (idempotent), inside tx, leaving a gap
// in the positions (the order needs none closed). It reports whether a row was
// removed.
func (l orderedList) drop(ctx context.Context, tx listTx, owner int64, ref Ref) (bool, error) {
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

// attachBooks sets each item's Book from the index (booksAt), leaving it nil
// where no book is indexed at the path.
func (c *Catalog) attachBooks(ctx context.Context, items []ListItem) error {
	refs := make([]Ref, len(items))
	for i, it := range items {
		refs[i] = it.Ref
	}
	books, err := c.booksAt(ctx, refs)
	if err != nil {
		return err
	}
	for i := range items {
		if b, ok := books[items[i].Ref]; ok {
			items[i].Book = &b
		}
	}
	return nil
}

// booksAt reads the books indexed at refs (BooksByPaths: one chunked read per
// library), keyed by ref; a ref with no book is absent.
func (c *Catalog) booksAt(ctx context.Context, refs []Ref) (map[Ref]Book, error) {
	byLib := map[int64][]string{}
	for _, r := range refs {
		byLib[r.LibraryID] = append(byLib[r.LibraryID], r.Path)
	}
	out := make(map[Ref]Book, len(refs))
	for libID, paths := range byLib {
		found, err := c.BooksByPaths(ctx, libID, paths)
		if err != nil {
			return nil, err
		}
		for p, b := range found {
			out[Ref{LibraryID: libID, Path: p}] = b
		}
	}
	return out, nil
}

// listableRefs is the skip rule of a whole-list replace: refs in order, each
// path cleaned (CleanRelPath), duplicates collapsed (the first wins), keeping only
// those that name exactly an indexed book (no part path, no indexing on demand)
// the caller's current access allows (scopes). Nothing skipped is an error.
func (c *Catalog) listableRefs(ctx context.Context, refs []Ref, scopes []Scope) ([]Ref, error) {
	seen := make(map[Ref]bool, len(refs))
	var cands []Ref
	for _, r := range refs {
		r.Path = CleanRelPath(r.Path)
		if r.Path == "" || seen[r] || !scopesAllow(scopes, r) {
			continue
		}
		seen[r] = true
		cands = append(cands, r)
	}
	books, err := c.booksAt(ctx, cands)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(cands))
	for _, r := range cands {
		if _, ok := books[r]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// stamp is the time lists and collections record (added_at, created_at,
// updated_at): UTC, fixed width with milliseconds, so the newest-first orders
// compare it as text.
func (c *Catalog) stamp() string { return formatSessionTime(c.now()) }
