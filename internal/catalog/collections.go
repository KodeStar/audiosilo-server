package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Collections: a user's own named, ordered lists of books (their items are an
// orderedList, like the up-next queue), which the owner may share READ-ONLY with
// named users on the same server. Every reader, the owner included, sees only
// the items their OWN current access allows: a viewer never sees, nor counts, an
// owner's item outside the viewer's shares. A collection the caller neither owns
// nor was shared reads as ErrNotFound (never a "forbidden", which would confirm it
// exists); a viewer's write is ErrNotOwner.

// Collection limits.
const (
	MaxCollections           = 100  // owned per user
	MaxCollectionItems       = 1000 // items per collection
	MaxCollectionShares      = 50   // users a collection is shared with
	MaxCollectionName        = 100  // characters, after trimming
	MaxCollectionDescription = 1000 // characters, after trimming
	collectionPreviewSize    = 4    // books in Collection.Preview
)

var (
	// ErrCollectionsFull is a create by a user owning MaxCollections already.
	ErrCollectionsFull = errors.New("too many collections")
	// ErrNotOwner is a write to a collection by a user it is only shared with.
	ErrNotOwner = errors.New("only the collection's owner can change it")
	// ErrInvalidName is a collection name that is empty or too long after
	// trimming, or holds control characters or invalid UTF-8.
	ErrInvalidName = errors.New("invalid collection name")
	// ErrInvalidDescription is a description over the limit, or holding control
	// characters (other than line breaks and tabs) or invalid UTF-8.
	ErrInvalidDescription = errors.New("invalid collection description")
	// ErrUnknownUser is a share naming a user that does not exist, is the owner,
	// or is disabled or a demo account (and not already shared with: a viewer
	// disabled since keeps their share).
	ErrUnknownUser = errors.New("unknown user")
	// ErrTooManyShares is a share list longer than MaxCollectionShares.
	ErrTooManyShares = errors.New("too many users")
)

// CollectionUser is a user as a collection shows them: the owner, a viewer, a
// share target.
type CollectionUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Collection is a collection as its reader sees it. SharedWith is set for the
// owner only (empty when unshared, absent for a viewer). ItemCount and Preview
// (the first collectionPreviewSize indexed items, for a cover mosaic) count only
// the items the reader's access allows.
type Collection struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Owner       CollectionUser    `json:"owner"`
	Owned       bool              `json:"owned"`
	SharedWith  *[]CollectionUser `json:"shared_with,omitempty"`
	ItemCount   int               `json:"item_count"`
	Preview     []Book            `json:"preview"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
}

// collectionItems is a collection's items as an ordered list, owned by the
// collection.
var collectionItems = orderedList{
	max:     MaxCollectionItems,
	full:    ErrCollectionFull,
	load:    `SELECT library_id, rel_path, added_at, position FROM collection_items WHERE collection_id = ?` + orderBy,
	visible: `SELECT library_id, rel_path, added_at FROM collection_items WHERE collection_id = ? AND `,
	insert:  `INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,?)`,
	setPos:  `UPDATE collection_items SET position = ? WHERE collection_id = ? AND library_id = ? AND rel_path = ?`,
	shift:   `UPDATE collection_items SET position = position + 1 WHERE collection_id = ? AND position >= ?`,
	remove:  `DELETE FROM collection_items WHERE collection_id = ? AND library_id = ? AND rel_path = ?`,
}

// cleanCollectionName trims a collection name and checks it: 1 to 100
// characters, valid UTF-8, no control characters. ErrInvalidName otherwise.
func cleanCollectionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > MaxCollectionName || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return name, nil
}

// cleanCollectionDescription trims a description and checks it: at most 1000
// characters, valid UTF-8, no control characters but line breaks and tabs.
// ErrInvalidDescription otherwise.
func cleanCollectionDescription(desc string) (string, error) {
	desc = strings.TrimSpace(desc)
	bad := func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }
	if utf8.RuneCountInString(desc) > MaxCollectionDescription || !utf8.ValidString(desc) || strings.ContainsFunc(desc, bad) {
		return "", ErrInvalidDescription
	}
	return desc, nil
}

// collectionRole reports whether userID owns collection id (true) or was shared
// it (false). Neither, or no such collection, is ErrNotFound.
func collectionRole(ctx context.Context, q querier, id, userID int64) (owned bool, err error) {
	var shared bool
	err = q.QueryRowContext(ctx,
		`SELECT c.user_id = ?1,
		        EXISTS(SELECT 1 FROM collection_shares s WHERE s.collection_id = c.id AND s.user_id = ?1)
		   FROM collections c WHERE c.id = ?2`, userID, id).Scan(&owned, &shared)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case !owned && !shared:
		return false, ErrNotFound
	}
	return owned, nil
}

// requireOwner is nil when userID owns collection id, ErrNotOwner when it was
// only shared with them, ErrNotFound otherwise.
func requireOwner(ctx context.Context, q querier, id, userID int64) error {
	owned, err := collectionRole(ctx, q, id, userID)
	if err == nil && !owned {
		err = ErrNotOwner
	}
	return err
}

// RequireCollectionOwner is nil when userID owns collection id, ErrNotOwner for a
// viewer, ErrNotFound for anyone else. A handler checks it before resolving a
// book to add, so a stranger or a viewer triggers no read or indexing.
func (c *Catalog) RequireCollectionOwner(ctx context.Context, id, userID int64) error {
	return requireOwner(ctx, c.db, id, userID)
}

// collectionCols selects a collection as a reader sees it (the one parameter: the
// reader, for owned).
const collectionCols = `SELECT c.id, c.name, c.description, c.user_id, u.username, c.user_id = ?, c.created_at, c.updated_at
	   FROM collections c JOIN users u ON u.id = c.user_id`

// Collections returns the collections userID owns, then those shared with them,
// each group newest updated_at first, as they see them (scopes, their
// UserScopes, filter the counts and previews).
func (c *Catalog) Collections(ctx context.Context, userID int64, scopes []Scope) ([]Collection, error) {
	cols, err := c.collectionRows(ctx, collectionCols+`
	  WHERE c.user_id = ? OR c.id IN (SELECT collection_id FROM collection_shares WHERE user_id = ?)
	  ORDER BY c.user_id = ? DESC, c.updated_at DESC, c.id DESC`, userID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	if err := c.countAndPreview(ctx, cols, scopes); err != nil {
		return nil, err
	}
	return cols, c.attachShares(ctx, cols)
}

// Collection returns one collection as userID sees it (see Collections), or
// ErrNotFound when they neither own it nor were shared it.
func (c *Catalog) Collection(ctx context.Context, id, userID int64, scopes []Scope) (*Collection, error) {
	col, err := c.readableCollection(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	cols := []Collection{*col}
	if err := c.countAndPreview(ctx, cols, scopes); err != nil {
		return nil, err
	}
	if err := c.attachShares(ctx, cols); err != nil {
		return nil, err
	}
	return &cols[0], nil
}

// CollectionDetail is Collection plus its items as userID sees them: in order,
// only those their current access allows, each with its book when indexed. The
// count and the preview come from those items.
func (c *Catalog) CollectionDetail(ctx context.Context, id, userID int64, scopes []Scope) (*Collection, []ListItem, error) {
	col, err := c.readableCollection(ctx, id, userID)
	if err != nil {
		return nil, nil, err
	}
	items, err := c.visibleItems(ctx, collectionItems, id, scopes)
	if err != nil {
		return nil, nil, err
	}
	col.ItemCount = len(items)
	for _, it := range items {
		if it.Book != nil && len(col.Preview) < collectionPreviewSize {
			col.Preview = append(col.Preview, *it.Book)
		}
	}
	if err := c.attachShares(ctx, []Collection{*col}); err != nil {
		return nil, nil, err
	}
	return col, items, nil
}

// readableCollection reads collection id as userID sees it, undecorated (no count,
// preview or shares): their own or one shared with them, else ErrNotFound.
func (c *Catalog) readableCollection(ctx context.Context, id, userID int64) (*Collection, error) {
	cols, err := c.collectionRows(ctx, collectionCols+`
	  WHERE c.id = ? AND (c.user_id = ? OR
	        EXISTS(SELECT 1 FROM collection_shares s WHERE s.collection_id = c.id AND s.user_id = ?))`,
		userID, id, userID, userID)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, ErrNotFound
	}
	return &cols[0], nil
}

// collectionRows reads collections by a query selecting collectionCols. An owned
// one gets an empty SharedWith, which attachShares fills (a viewer's stays nil:
// never shown).
func (c *Catalog) collectionRows(ctx context.Context, query string, args ...any) ([]Collection, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, col *Collection) error {
		col.Preview = []Book{}
		if err := rows.Scan(&col.ID, &col.Name, &col.Description, &col.Owner.ID, &col.Owner.Username,
			&col.Owned, &col.CreatedAt, &col.UpdatedAt); err != nil {
			return err
		}
		if col.Owned {
			col.SharedWith = &[]CollectionUser{}
		}
		return nil
	}, query, args...)
}

// idChunk bounds the ids one IN (...) list binds.
const idChunk = 500

// inChunks calls read with each chunk of ids as an IN list's placeholders and
// arguments.
func inChunks(ids []int64, read func(in string, args []any) error) error {
	for start := 0; start < len(ids); start += idChunk {
		part := ids[start:min(start+idChunk, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		if err := read(placeholders(len(args)), args); err != nil {
			return err
		}
	}
	return nil
}

// countAndPreview fills cols' item counts and previews (the first
// collectionPreviewSize indexed items) as the reader sees them (scopes).
func (c *Catalog) countAndPreview(ctx context.Context, cols []Collection, scopes []Scope) error {
	byID := make(map[int64]*Collection, len(cols))
	ids := make([]int64, len(cols))
	for i := range cols {
		byID[cols[i].ID], ids[i] = &cols[i], cols[i].ID
	}
	filter, fargs := scopesFilterSQL("ci.library_id", "ci.rel_path", scopes)
	if err := inChunks(ids, func(in string, args []any) error {
		type countRow struct{ id, n int64 }
		counts, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *countRow) error {
			return rows.Scan(&r.id, &r.n)
		}, `SELECT ci.collection_id, COUNT(*) FROM collection_items ci
		  WHERE ci.collection_id IN (`+in+`) AND `+filter+` GROUP BY ci.collection_id`, append(args, fargs...)...)
		if err != nil {
			return err
		}
		for _, r := range counts {
			byID[r.id].ItemCount = int(r.n)
		}
		return nil
	}); err != nil {
		return err
	}

	// The previews, one bounded read per collection: its items in stored order
	// (idx_collection_items_order), stopping at the first collectionPreviewSize
	// indexed ones the reader can see, rather than ranking every item of every
	// collection to keep four of each. INDEXED BY because a path-scoped reader's
	// filter otherwise leads the planner to the primary key (an OR of path
	// ranges), which reads every visible item and sorts them all for the four.
	preview := `SELECT ci.library_id, ci.rel_path FROM collection_items ci INDEXED BY idx_collection_items_order
	   JOIN books b ON b.library_id = ci.library_id AND b.rel_path = ci.rel_path
	  WHERE ci.collection_id = ? AND ` + filter + `
	  ORDER BY ci.position, ci.library_id, ci.rel_path LIMIT ?`
	var (
		refs []Ref
		of   []*Collection // the collection each of refs previews
	)
	for i := range cols {
		col := &cols[i]
		args := append(append([]any{col.ID}, fargs...), collectionPreviewSize)
		got, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *Ref) error {
			return rows.Scan(&r.LibraryID, &r.Path)
		}, preview, args...)
		if err != nil {
			return err
		}
		for _, r := range got {
			refs, of = append(refs, r), append(of, col)
		}
	}
	books, err := c.booksAt(ctx, refs)
	if err != nil {
		return err
	}
	for i, r := range refs {
		if b, ok := books[r]; ok {
			of[i].Preview = append(of[i].Preview, b)
		}
	}
	return nil
}

// attachShares fills who the reader's own collections among cols are shared
// with (collectionRows gave each an empty SharedWith).
func (c *Catalog) attachShares(ctx context.Context, cols []Collection) error {
	shared := map[int64]*[]CollectionUser{}
	var ids []int64
	for _, col := range cols {
		if col.Owned {
			shared[col.ID] = col.SharedWith
			ids = append(ids, col.ID)
		}
	}
	return inChunks(ids, func(in string, args []any) error {
		type shareRow struct {
			id int64
			CollectionUser
		}
		shares, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *shareRow) error {
			return rows.Scan(&r.id, &r.ID, &r.Username)
		}, `SELECT s.collection_id, u.id, u.username FROM collection_shares s JOIN users u ON u.id = s.user_id
		  WHERE s.collection_id IN (`+in+`) ORDER BY u.username, u.id`, args...)
		if err != nil {
			return err
		}
		for _, sh := range shares {
			*shared[sh.id] = append(*shared[sh.id], sh.CollectionUser)
		}
		return nil
	})
}

// CreateCollection makes a collection owned by userID (name and description
// cleaned by cleanCollectionName/cleanCollectionDescription) and returns it as
// its owner sees it (built from what the insert wrote: no items, no shares). An
// owner of MaxCollections already is ErrCollectionsFull.
func (c *Catalog) CreateCollection(ctx context.Context, userID int64, name, description string) (*Collection, error) {
	name, err := cleanCollectionName(name)
	if err != nil {
		return nil, err
	}
	if description, err = cleanCollectionDescription(description); err != nil {
		return nil, err
	}
	col := &Collection{Name: name, Description: description, Owner: CollectionUser{ID: userID}, Owned: true,
		SharedWith: &[]CollectionUser{}, Preview: []Book{}}
	if err := c.db.WithTx(ctx, "CreateCollection", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT username, (SELECT COUNT(*) FROM collections WHERE user_id = ?1) FROM users WHERE id = ?1`,
			userID).Scan(&col.Owner.Username, &n); err != nil {
			return err
		}
		if n >= MaxCollections {
			return ErrCollectionsFull
		}
		col.CreatedAt = c.stamp()
		col.UpdatedAt = col.CreatedAt
		res, err := tx.ExecContext(ctx,
			`INSERT INTO collections(user_id, name, description, created_at, updated_at) VALUES(?,?,?,?,?)`,
			userID, name, description, col.CreatedAt, col.UpdatedAt)
		if err != nil {
			return err
		}
		col.ID, err = res.LastInsertId()
		return err
	}); err != nil {
		return nil, err
	}
	return col, nil
}

// UpdateCollection renames a collection and/or changes its description (nil
// leaves a field), owner only; updated_at moves when either is given. Who may
// change it is settled before the input is checked, so a stranger is ErrNotFound
// and a viewer ErrNotOwner whatever the body holds.
func (c *Catalog) UpdateCollection(ctx context.Context, id, userID int64, name, description *string) error {
	return c.db.WithTx(ctx, "UpdateCollection", func(tx *sql.Tx) error {
		if err := requireOwner(ctx, tx, id, userID); err != nil {
			return err
		}
		var err error
		var n, d string
		if name != nil {
			if n, err = cleanCollectionName(*name); err != nil {
				return err
			}
		}
		if description != nil {
			if d, err = cleanCollectionDescription(*description); err != nil {
				return err
			}
		}
		if name == nil && description == nil {
			return nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE collections SET name = CASE WHEN ?1 THEN ?2 ELSE name END,
			        description = CASE WHEN ?3 THEN ?4 ELSE description END, updated_at = ?5
			  WHERE id = ?6`, name != nil, n, description != nil, d, c.stamp(), id)
		return err
	})
}

// DeleteCollection deletes a collection its owner calls it for (its items and
// shares go with it), or takes a viewer off it (their share only: they leave).
func (c *Catalog) DeleteCollection(ctx context.Context, id, userID int64) error {
	return c.db.WithTx(ctx, "DeleteCollection", func(tx *sql.Tx) error {
		owned, err := collectionRole(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		if owned {
			_, err = tx.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM collection_shares WHERE collection_id = ? AND user_id = ?`, id, userID)
		}
		return err
	})
}

// changeItems runs one change to a collection's items for its owner, inside a
// transaction, and moves its updated_at when the items changed.
func (c *Catalog) changeItems(ctx context.Context, op string, id, userID int64,
	change func(tx *sql.Tx, now string) (bool, error)) error {
	return c.db.WithTx(ctx, op, func(tx *sql.Tx) error {
		if err := requireOwner(ctx, tx, id, userID); err != nil {
			return err
		}
		now := c.stamp()
		changed, err := change(tx, now)
		if err != nil || !changed {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE collections SET updated_at = ? WHERE id = ?`, now, id)
		return err
	})
}

// AddCollectionItem adds a book (ref, its own path: the caller resolves and
// authorizes it) to a collection userID owns, at position, an index in the items
// as the owner sees them with scopes (their UserScopes; see orderedList.add). A
// collection whose visible items number MaxCollectionItems is ErrCollectionFull
// (an ErrListFull); one full only because of hidden items loses the oldest of
// them instead.
func (c *Catalog) AddCollectionItem(ctx context.Context, id, userID int64, ref Ref, position *int, scopes []Scope) error {
	return c.changeItems(ctx, "AddCollectionItem", id, userID, func(tx *sql.Tx, now string) (bool, error) {
		return collectionItems.add(ctx, tx, id, ref, position, scopes, now)
	})
}

// SetCollectionItems replaces the items of a collection userID owns with refs, by
// the skip rule (listableRefs, against the owner's scopes). More than
// MaxCollectionItems refs is ErrTooManyItems.
func (c *Catalog) SetCollectionItems(ctx context.Context, id, userID int64, refs []Ref, scopes []Scope) error {
	if err := requireOwner(ctx, c.db, id, userID); err != nil {
		return err // answer a stranger or a viewer before reading anything for them
	}
	if err := collectionItems.fits(refs); err != nil {
		return err
	}
	keep, err := c.listableRefs(ctx, refs, scopes)
	if err != nil {
		return err
	}
	return c.changeItems(ctx, "SetCollectionItems", id, userID, func(tx *sql.Tx, now string) (bool, error) {
		return collectionItems.replace(ctx, tx, id, keep, now)
	})
}

// RemoveCollectionItem takes a book off a collection userID owns (idempotent; no
// access check on the path, so a revoked one can still be cleaned up).
func (c *Catalog) RemoveCollectionItem(ctx context.Context, id, userID int64, ref Ref) error {
	return c.changeItems(ctx, "RemoveCollectionItem", id, userID, func(tx *sql.Tx, _ string) (bool, error) {
		return collectionItems.drop(ctx, tx, id, ref)
	})
}

// SetCollectionShares replaces who a collection userID owns is shared with. Every
// id must be an existing, enabled, non-demo user other than the owner, or one the
// collection is already shared with (a viewer disabled since stays in
// shared_with, so the list the owner was shown can be sent back), else
// ErrUnknownUser and nothing changes; more than MaxCollectionShares (after
// duplicates collapse) is ErrTooManyShares. Who may change it is settled first: a
// stranger is ErrNotFound and a viewer ErrNotOwner whatever the list holds.
func (c *Catalog) SetCollectionShares(ctx context.Context, id, userID int64, userIDs []int64) error {
	ids := slices.Compact(slices.Sorted(slices.Values(userIDs)))
	return c.db.WithTx(ctx, "SetCollectionShares", func(tx *sql.Tx) error {
		if err := requireOwner(ctx, tx, id, userID); err != nil {
			return err
		}
		if len(ids) > MaxCollectionShares {
			return ErrTooManyShares
		}
		// The ids go in as one JSON array (json_each), so the statements stay
		// constant: set-based, with nothing concatenated into SQL in a transaction.
		list, err := json.Marshal(append([]int64{}, ids...)) // [] for none, never null
		if err != nil {
			return err
		}
		var valid int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users
		  WHERE id != ?1 AND id IN (SELECT value FROM json_each(?2))
		    AND ((disabled = 0 AND is_demo = 0)
		         OR id IN (SELECT user_id FROM collection_shares WHERE collection_id = ?3))`,
			userID, string(list), id).Scan(&valid); err != nil {
			return err
		}
		if valid != len(ids) {
			return ErrUnknownUser
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM collection_shares
		  WHERE collection_id = ? AND user_id NOT IN (SELECT value FROM json_each(?))`, id, string(list)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO collection_shares(collection_id, user_id, created_at)
		  SELECT ?, value, ? FROM json_each(?)`, id, c.stamp(), string(list))
		return err
	})
}

// ShareTargets lists the users userID may share a collection with: enabled,
// non-demo accounts other than their own, by username.
func (c *Catalog) ShareTargets(ctx context.Context, userID int64) ([]CollectionUser, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, u *CollectionUser) error {
		return rows.Scan(&u.ID, &u.Username)
	}, `SELECT id, username FROM users WHERE disabled = 0 AND is_demo = 0 AND id != ? ORDER BY username, id`, userID)
}
