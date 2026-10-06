package catalog

import (
	"context"
	"database/sql"
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
	maxCollectionName        = 100  // characters, after trimming
	maxCollectionDescription = 1000 // characters, after trimming
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
	// ErrUnknownUser is a share naming a user that does not exist, is disabled, is
	// a demo account, or is the owner.
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
	load:    `SELECT library_id, rel_path, added_at, position FROM collection_items WHERE collection_id = ?` + orderBy,
	visible: `SELECT library_id, rel_path, added_at, position FROM collection_items WHERE collection_id = ? AND `,
	insert:  `INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,?)`,
	setPos:  `UPDATE collection_items SET position = ? WHERE collection_id = ? AND library_id = ? AND rel_path = ?`,
	remove:  `DELETE FROM collection_items WHERE collection_id = ? AND library_id = ? AND rel_path = ?`,
}

// CleanCollectionName trims a collection name and checks it: 1 to 100
// characters, valid UTF-8, no control characters. ErrInvalidName otherwise.
func CleanCollectionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > maxCollectionName || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return name, nil
}

// CleanCollectionDescription trims a description and checks it: at most 1000
// characters, valid UTF-8, no control characters but line breaks and tabs.
// ErrInvalidDescription otherwise.
func CleanCollectionDescription(desc string) (string, error) {
	desc = strings.TrimSpace(desc)
	bad := func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }
	if utf8.RuneCountInString(desc) > maxCollectionDescription || !utf8.ValidString(desc) || strings.ContainsFunc(desc, bad) {
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

// The collections a reader can open, and the fragments that pick a set of them
// for the decorating reads (counts, previews, shares): every collection the
// reader owns or was shared, or one collection by id.
const (
	collectionCols = `SELECT c.id, c.name, c.description, c.user_id, u.username, c.user_id = ?, c.created_at, c.updated_at
	   FROM collections c JOIN users u ON u.id = c.user_id`
	readableIDs = ` IN (SELECT id FROM collections WHERE user_id = ? UNION
	                    SELECT collection_id FROM collection_shares WHERE user_id = ?)`
	oneID = ` = ?`
)

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
	return cols, c.decorate(ctx, cols, userID, scopes, readableIDs, userID, userID)
}

// Collection returns one collection as userID sees it (see Collections), or
// ErrNotFound when they neither own it nor were shared it.
func (c *Catalog) Collection(ctx context.Context, id, userID int64, scopes []Scope) (*Collection, error) {
	if _, err := collectionRole(ctx, c.db, id, userID); err != nil {
		return nil, err
	}
	cols, err := c.collectionRows(ctx, collectionCols+` WHERE c.id = ?`, userID, id)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 { // deleted since the role check
		return nil, ErrNotFound
	}
	if err := c.decorate(ctx, cols, userID, scopes, oneID, id); err != nil {
		return nil, err
	}
	return &cols[0], nil
}

// CollectionDetail is Collection plus its items as userID sees them: in order,
// only those their current access allows, each with its book when indexed.
func (c *Catalog) CollectionDetail(ctx context.Context, id, userID int64, scopes []Scope) (*Collection, []ListItem, error) {
	col, err := c.Collection(ctx, id, userID, scopes)
	if err != nil {
		return nil, nil, err
	}
	items, err := c.visibleItems(ctx, collectionItems, id, scopes)
	if err != nil {
		return nil, nil, err
	}
	return col, items, nil
}

// collectionRows reads collections by a query selecting collectionCols.
func (c *Catalog) collectionRows(ctx context.Context, query string, args ...any) ([]Collection, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, col *Collection) error {
		col.Preview = []Book{}
		return rows.Scan(&col.ID, &col.Name, &col.Description, &col.Owner.ID, &col.Owner.Username,
			&col.Owned, &col.CreatedAt, &col.UpdatedAt)
	}, query, args...)
}

// decorate fills cols' item counts and previews as the reader sees them (scopes)
// and, on the ones the reader owns, who they are shared with. which (readableIDs
// or oneID, with its args) picks the collections cols holds.
func (c *Catalog) decorate(ctx context.Context, cols []Collection, readerID int64, scopes []Scope, which string, whichArgs ...any) error {
	if len(cols) == 0 {
		return nil
	}
	byID := make(map[int64]*Collection, len(cols))
	for i := range cols {
		byID[cols[i].ID] = &cols[i]
		if cols[i].Owned {
			cols[i].SharedWith = &[]CollectionUser{}
		}
	}
	filter, fargs := scopesFilterSQL("ci.library_id", "ci.rel_path", scopes)
	args := append(slices.Clone(whichArgs), fargs...)

	// Visible items per collection.
	type countRow struct{ id, n int64 }
	counts, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *countRow) error {
		return rows.Scan(&r.id, &r.n)
	}, `SELECT ci.collection_id, COUNT(*) FROM collection_items ci
	  WHERE ci.collection_id`+which+` AND `+filter+` GROUP BY ci.collection_id`, args...)
	if err != nil {
		return err
	}
	for _, r := range counts {
		if col := byID[r.id]; col != nil {
			col.ItemCount = int(r.n)
		}
	}

	// The first visible indexed items of each, for the preview.
	type previewRow struct {
		id int64
		Ref
	}
	prev, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *previewRow) error {
		return rows.Scan(&r.id, &r.LibraryID, &r.Path)
	}, `SELECT collection_id, library_id, rel_path FROM (
	      SELECT ci.collection_id, ci.library_id, ci.rel_path,
	             ROW_NUMBER() OVER (PARTITION BY ci.collection_id
	                                ORDER BY ci.position, ci.library_id, ci.rel_path) AS rn
	        FROM collection_items ci
	        JOIN books b ON b.library_id = ci.library_id AND b.rel_path = ci.rel_path
	       WHERE ci.collection_id`+which+` AND `+filter+`)
	  WHERE rn <= ? ORDER BY collection_id, rn`, append(args, collectionPreviewSize)...)
	if err != nil {
		return err
	}
	items := make([]ListItem, len(prev))
	for i, p := range prev {
		items[i].Ref = p.Ref
	}
	if err := c.attachBooks(ctx, items); err != nil {
		return err
	}
	for i, p := range prev {
		if col := byID[p.id]; col != nil && items[i].Book != nil {
			col.Preview = append(col.Preview, *items[i].Book)
		}
	}

	// Who the reader's own collections are shared with (never a viewer's).
	type shareRow struct {
		id int64
		CollectionUser
	}
	shares, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *shareRow) error {
		return rows.Scan(&r.id, &r.ID, &r.Username)
	}, `SELECT s.collection_id, u.id, u.username FROM collection_shares s JOIN users u ON u.id = s.user_id
	  WHERE s.collection_id`+which+` AND s.collection_id IN (SELECT id FROM collections WHERE user_id = ?)
	  ORDER BY u.username, u.id`, append(slices.Clone(whichArgs), readerID)...)
	if err != nil {
		return err
	}
	for _, s := range shares {
		if col := byID[s.id]; col != nil && col.SharedWith != nil {
			*col.SharedWith = append(*col.SharedWith, s.CollectionUser)
		}
	}
	return nil
}

// CreateCollection makes a collection owned by userID (name and description
// cleaned by CleanCollectionName/CleanCollectionDescription) and returns it. An
// owner of MaxCollections already is ErrCollectionsFull.
func (c *Catalog) CreateCollection(ctx context.Context, userID int64, name, description string) (*Collection, error) {
	name, err := CleanCollectionName(name)
	if err != nil {
		return nil, err
	}
	if description, err = CleanCollectionDescription(description); err != nil {
		return nil, err
	}
	var id int64
	if err := c.db.WithTx(ctx, "CreateCollection", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM collections WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxCollections {
			return ErrCollectionsFull
		}
		now := c.stamp()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO collections(user_id, name, description, created_at, updated_at) VALUES(?,?,?,?,?)`,
			userID, name, description, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		return nil, err
	}
	return c.Collection(ctx, id, userID, nil)
}

// UpdateCollection renames a collection and/or changes its description (nil
// leaves a field), owner only; updated_at moves when either is given.
func (c *Catalog) UpdateCollection(ctx context.Context, id, userID int64, name, description *string) error {
	var err error
	var n, d string
	if name != nil {
		if n, err = CleanCollectionName(*name); err != nil {
			return err
		}
	}
	if description != nil {
		if d, err = CleanCollectionDescription(*description); err != nil {
			return err
		}
	}
	return c.db.WithTx(ctx, "UpdateCollection", func(tx *sql.Tx) error {
		if err := requireOwner(ctx, tx, id, userID); err != nil {
			return err
		}
		if name == nil && description == nil {
			return nil
		}
		_, err := tx.ExecContext(ctx,
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
// authorizes it) to a collection userID owns, at position (orderedList.add). A
// full collection is ErrListFull.
func (c *Catalog) AddCollectionItem(ctx context.Context, id, userID int64, ref Ref, position *int) error {
	return c.changeItems(ctx, "AddCollectionItem", id, userID, func(tx *sql.Tx, now string) (bool, error) {
		return collectionItems.add(ctx, tx, id, ref, position, now)
	})
}

// SetCollectionItems replaces the items of a collection userID owns with refs, by
// the skip rule (listableRefs, against the owner's scopes). More than
// MaxCollectionItems refs is ErrTooManyItems.
func (c *Catalog) SetCollectionItems(ctx context.Context, id, userID int64, refs []Ref, scopes []Scope) error {
	if err := requireOwner(ctx, c.db, id, userID); err != nil {
		return err // answer a stranger or a viewer before reading anything for them
	}
	if len(refs) > MaxCollectionItems {
		return ErrTooManyItems
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
// id must be an existing, enabled, non-demo user other than the owner, else
// ErrUnknownUser and nothing changes; more than MaxCollectionShares (after
// duplicates collapse) is ErrTooManyShares.
func (c *Catalog) SetCollectionShares(ctx context.Context, id, userID int64, userIDs []int64) error {
	ids := slices.Compact(slices.Sorted(slices.Values(userIDs)))
	if len(ids) > MaxCollectionShares {
		return ErrTooManyShares
	}
	return c.db.WithTx(ctx, "SetCollectionShares", func(tx *sql.Tx) error {
		if err := requireOwner(ctx, tx, id, userID); err != nil {
			return err
		}
		for _, uid := range ids {
			if uid == userID {
				return ErrUnknownUser
			}
			var ok bool
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM users WHERE id = ? AND disabled = 0 AND is_demo = 0`, uid).Scan(&ok)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnknownUser
			}
			if err != nil {
				return err
			}
		}
		current, err := queryRows(ctx, tx, func(rows *sql.Rows, uid *int64) error {
			return rows.Scan(uid)
		}, `SELECT user_id FROM collection_shares WHERE collection_id = ?`, id)
		if err != nil {
			return err
		}
		for _, uid := range current {
			if _, found := slices.BinarySearch(ids, uid); found {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM collection_shares WHERE collection_id = ? AND user_id = ?`, id, uid); err != nil {
				return err
			}
		}
		now := c.stamp()
		for _, uid := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO collection_shares(collection_id, user_id, created_at) VALUES(?,?,?)`,
				id, uid, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ShareTargets lists the users userID may share a collection with: enabled,
// non-demo accounts other than their own, by username.
func (c *Catalog) ShareTargets(ctx context.Context, userID int64) ([]CollectionUser, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, u *CollectionUser) error {
		return rows.Scan(&u.ID, &u.Username)
	}, `SELECT id, username FROM users WHERE disabled = 0 AND is_demo = 0 AND id != ? ORDER BY username, id`, userID)
}
