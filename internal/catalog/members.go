package catalog

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/relpath"
)

// Household members (player redesign Phase 8b-1): a member is an account whose
// users.managed_by is its owner (auth/members.go keeps the accounts). Its access
// is the union of its own admin grants and what the owner gives it, decided here
// per request (no cache to invalidate):
//
//   - member_access 'all': the owner's CURRENT scopes, computed with the owner's
//     role (an admin owner gives every library, as content scope: the member
//     never gets the admin role);
//   - member_access 'collections': the books in collections the owner owns and
//     shares with the member, each kept only while the owner's current scopes
//     allow it (a book the owner lost access to drops out at once). A book is its
//     path, with relpath.Within semantics: a folder book covers its own subtree.
//
// One level only: a member's owner is never a member (auth refuses it); if the
// data says otherwise, nothing is derived from that owner. A disabled owner
// changes nothing (research G). Everything that reads access goes through
// UserScope, UserScopes or AccessibleLibraries (or their Caller forms, which the
// transport uses with the rule its token lookup read), so browse, search, media,
// cast, next book, the car and household listening all follow.

// ErrInvalidCollections is a member's collection_ids naming a collection that
// isn't the owner's own.
var ErrInvalidCollections = errors.New("collection_ids must all be your own collections")

// MemberRule is a household member's derivation: whose access (Owner), how much
// of it (Access: "all" | "collections") and whether that owner is an admin. The
// transport's comes from auth's token lookup (auth.MemberRule, read by the same
// columns' rule as memberRuleOf: all three member columns set, an owner who is
// neither the member nor a member).
type MemberRule struct {
	Owner      int64
	Access     string
	OwnerAdmin bool
}

// memberRuleOf reads userID's member rule, nil for anyone who isn't a member (or
// whose owner is themselves, or a member: nothing derives from those).
func (c *Catalog) memberRuleOf(ctx context.Context, userID int64) (*MemberRule, error) {
	var r MemberRule
	err := c.db.QueryRowContext(ctx,
		`SELECT u.managed_by, u.member_access, o.role = 'admin'
		   FROM users u JOIN users o ON o.id = u.managed_by
		  WHERE u.id = ? AND u.member_kind IS NOT NULL AND u.member_access IS NOT NULL
		    AND o.id <> u.id AND o.managed_by IS NULL`, userID).Scan(&r.Owner, &r.Access, &r.OwnerAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ownerScopes is the owner's current scopes by library (in libraryID only, or
// every library for 0): every library AllowAll for an admin, else their own
// grants (an owner is never a member, so nothing more).
func (c *Catalog) ownerScopes(ctx context.Context, owner int64, ownerAdmin bool, libraryID int64) (map[int64]*Scope, error) {
	if !ownerAdmin {
		return c.grantScopes(ctx, owner, libraryID)
	}
	out := map[int64]*Scope{}
	if libraryID > 0 {
		out[libraryID] = &Scope{LibraryID: libraryID, AllowAll: true}
		return out, nil
	}
	all, err := c.allLibraryScopes(ctx)
	for _, s := range all {
		out[s.LibraryID] = &Scope{LibraryID: s.LibraryID, AllowAll: true}
	}
	return out, err
}

// memberScopes is what a member's owner gives them (see the rules above), in
// libraryID only or every library for 0.
func (c *Catalog) memberScopes(ctx context.Context, memberID, libraryID int64, r *MemberRule) ([]Scope, error) {
	owner, err := c.ownerScopes(ctx, r.Owner, r.OwnerAdmin, libraryID)
	if err != nil {
		return nil, err
	}
	if r.Access == "all" {
		return flattenScopes(owner), nil
	}
	refs, err := queryRows(ctx, c.db, func(rows *sql.Rows, ref *Ref) error {
		return rows.Scan(&ref.LibraryID, &ref.Path)
	}, `SELECT DISTINCT ci.library_id, ci.rel_path
	      FROM collection_shares s
	      JOIN collections col ON col.id = s.collection_id AND col.user_id = ?1
	      JOIN collection_items ci ON ci.collection_id = col.id
	     WHERE s.user_id = ?2 AND (?3 = 0 OR ci.library_id = ?3)`, r.Owner, memberID, libraryID)
	if err != nil {
		return nil, err
	}
	byLib := map[int64]*Scope{}
	for _, ref := range refs {
		// A book is a non-empty, canonical path ("" would be the whole library).
		if ref.Path == "" || relpath.Clean(ref.Path) != ref.Path {
			continue
		}
		if s := owner[ref.LibraryID]; s == nil || !s.Allows(ref.Path) {
			continue
		}
		s := byLib[ref.LibraryID]
		if s == nil {
			s = &Scope{LibraryID: ref.LibraryID}
			byLib[ref.LibraryID] = s
		}
		s.Paths = append(s.Paths, ref.Path)
	}
	return flattenScopes(byLib), nil
}

// SetMemberCollections makes ids the full set of owner's collections shared
// with member, inside tx (a member create or change, auth.ShareFunc): a share on
// each listed collection, and the member taken off every other collection owner
// owns; collections owned by anyone else are never touched. An id that isn't one
// of owner's collections is ErrInvalidCollections and a listed collection that
// would pass MaxCollectionShares is ErrTooManyShares, both before anything
// changes. An empty ids takes the member off all of owner's collections.
func (c *Catalog) SetMemberCollections(ctx context.Context, tx *sql.Tx, owner, member int64, ids []int64) error {
	ids, list, err := idList(ids)
	if err != nil {
		return err
	}
	var own, full int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(NOT EXISTS(SELECT 1 FROM collection_shares s WHERE s.collection_id = c.id AND s.user_id = ?3)
		                     AND (SELECT COUNT(*) FROM collection_shares s WHERE s.collection_id = c.id) >= ?4), 0)
		   FROM collections c WHERE c.user_id = ?1 AND c.id IN (SELECT value FROM json_each(?2))`,
		owner, string(list), member, MaxCollectionShares).Scan(&own, &full); err != nil {
		return err
	}
	if own != len(ids) {
		return ErrInvalidCollections
	}
	if full > 0 {
		return ErrTooManyShares
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collection_shares
		  WHERE user_id = ?1 AND collection_id IN (SELECT id FROM collections WHERE user_id = ?2)
		    AND collection_id NOT IN (SELECT value FROM json_each(?3))`, member, owner, string(list)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO collection_shares(collection_id, user_id, created_at)
		 SELECT value, ?, ? FROM json_each(?)`, member, c.stamp(), string(list))
	return err
}

// MemberCollection is one of an owner's collections shared with a member, with
// its item count as the owner's current access sees it.
type MemberCollection struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// MemberCollections maps each of members to the collections owner owns and
// shares with them, in the owner's own order (newest updated first, as
// Collections lists them), each item count filtered by the owner's current
// scopes (ownerScopes: ownerAdmin is the owner's role). A member with none is
// absent.
func (c *Catalog) MemberCollections(ctx context.Context, owner int64, ownerAdmin bool, members []int64) (map[int64][]MemberCollection, error) {
	out := map[int64][]MemberCollection{}
	type row struct {
		member int64
		MemberCollection
	}
	var shared []row
	if err := inChunks(members, func(in string, args []any) error {
		rows, err := queryRows(ctx, c.db, func(rs *sql.Rows, r *row) error {
			return rs.Scan(&r.member, &r.ID, &r.Name)
		}, `SELECT s.user_id, col.id, col.name
		      FROM collection_shares s JOIN collections col ON col.id = s.collection_id
		     WHERE col.user_id = ? AND s.user_id IN (`+in+`)
		     ORDER BY col.updated_at DESC, col.id DESC`, append([]any{owner}, args...)...)
		shared = append(shared, rows...)
		return err
	}); err != nil || len(shared) == 0 {
		return out, err
	}
	byLib, err := c.ownerScopes(ctx, owner, ownerAdmin, 0)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(shared))
	for _, r := range shared {
		ids = append(ids, r.ID)
	}
	counts, err := c.scopedItemCounts(ctx, slices.Compact(slices.Sorted(slices.Values(ids))), flattenScopes(byLib))
	if err != nil {
		return nil, err
	}
	for _, r := range shared {
		r.Items = counts[r.ID]
		out[r.member] = append(out[r.member], r.MemberCollection)
	}
	return out, nil
}

// LastListened maps each of users to the end of their newest listening session
// recorded here (not imported, not backfilled, with listening in it): fixed-width
// millisecond UTC. Someone who never listened is absent.
func (c *Catalog) LastListened(ctx context.Context, users []int64) (map[int64]string, error) {
	out := map[int64]string{}
	err := inChunks(users, func(in string, args []any) error {
		type row struct {
			id int64
			at string
		}
		rows, err := queryRows(ctx, c.db, func(rs *sql.Rows, r *row) error {
			return rs.Scan(&r.id, &r.at)
		}, `SELECT s.user_id, MAX(s.last_at) FROM listening_sessions s
		     WHERE s.user_id IN (`+in+`) AND s.import_id = 0 AND s.backfilled = 0 AND `+listenedSQL+`
		     GROUP BY s.user_id`, args...)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out[r.id] = r.at
		}
		return nil
	})
	return out, err
}
