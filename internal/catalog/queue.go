package catalog

import (
	"context"
	"database/sql"
)

// MaxQueue is how many books a user's up-next queue holds at most.
const MaxQueue = 500

// upNext is the up-next queue as an ordered list, owned by a user.
var upNext = orderedList{
	max:     MaxQueue,
	full:    ErrQueueFull,
	load:    `SELECT library_id, rel_path, added_at, position FROM up_next WHERE user_id = ?` + orderBy,
	visible: `SELECT library_id, rel_path, added_at, position FROM up_next WHERE user_id = ? AND `,
	insert:  `INSERT INTO up_next(user_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,?)`,
	setPos:  `UPDATE up_next SET position = ? WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
	shift:   `UPDATE up_next SET position = position + 1 WHERE user_id = ? AND position >= ?`,
	remove:  `DELETE FROM up_next WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
}

// Queue returns a user's up-next queue in order: only the books their current
// access allows (scopes, from UserScopes); entries under a since-revoked share are
// kept but not returned, and come back if access does.
func (c *Catalog) Queue(ctx context.Context, userID int64, scopes []Scope) ([]ListItem, error) {
	return c.visibleItems(ctx, upNext, userID, scopes)
}

// AddToQueue queues a book (ref, its own path: the caller resolves and authorizes
// it) at position (see orderedList.add): an already-queued book moves only when a
// position is given. A full queue is ErrQueueFull (an ErrListFull).
func (c *Catalog) AddToQueue(ctx context.Context, userID int64, ref Ref, position *int) error {
	return c.db.WithTx(ctx, "AddToQueue", func(tx *sql.Tx) error {
		_, err := upNext.add(ctx, tx, userID, ref, position, c.stamp())
		return err
	})
}

// SetQueue replaces a user's whole queue with refs, in order, by the skip rule
// (listableRefs: only exactly indexed books in scopes, duplicates collapsed).
// Stored entries not in the result are deleted, hidden (out-of-scope) ones
// included. More than MaxQueue refs is ErrTooManyItems.
func (c *Catalog) SetQueue(ctx context.Context, userID int64, refs []Ref, scopes []Scope) error {
	if err := upNext.fits(refs); err != nil {
		return err
	}
	keep, err := c.listableRefs(ctx, refs, scopes)
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "SetQueue", func(tx *sql.Tx) error {
		_, err := upNext.replace(ctx, tx, userID, keep, c.stamp())
		return err
	})
}

// RemoveFromQueue takes a book off a user's queue (idempotent). It needs no
// access check: it only removes the user's own row, so a revoked path can still
// be cleaned up.
func (c *Catalog) RemoveFromQueue(ctx context.Context, userID int64, ref Ref) error {
	return c.db.WithTx(ctx, "RemoveFromQueue", func(tx *sql.Tx) error {
		_, err := upNext.drop(ctx, tx, userID, ref)
		return err
	})
}
