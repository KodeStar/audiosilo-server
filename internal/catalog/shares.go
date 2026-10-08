package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// Sharing is filesystem-based: a Share is a named set of path rules, where each
// rule's Path is any node in a library's tree ("" = whole library, an author or
// series folder = subtree, a book = single item). Users are granted shares; a
// user's access to a path is the union of all their granted rules.

// Share is a named, grantable bundle of path rules.
type Share struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// ReadOnly is reserved/forward-looking: it is persisted and editable but not
	// yet enforced anywhere (the content API exposes no per-share write path, so
	// all share access is already read-only). Gate a future write/upload path on
	// it when one exists.
	ReadOnly bool       `json:"read_only"`
	Paths    []PathRule `json:"paths,omitempty"`
	// WholeLibraryID is set on the shares GrantWholeLibrary makes: the library
	// they grant whole. Clients list those as library access, not named shares.
	WholeLibraryID *int64 `json:"whole_library_id,omitempty"`
}

// PathRule grants a path (and everything under it) within a library.
type PathRule struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"` // "" = whole library
}

// Scope is a user's effective access within a single library: either the whole
// library (AllowAll) or a set of granted subtree/item paths.
type Scope struct {
	LibraryID int64
	AllowAll  bool
	Paths     []string
}

// CleanRelPath canonicalizes a library-relative path: separators become slashes,
// "." and ".." segments collapse (a leading ".." clamps at the root instead of
// escaping it), and enclosing slashes are trimmed - "" addresses the library
// root. Security-relevant: pathAllowedBy is a literal prefix match, so without
// prior normalization "Author A/../Author B/x.m4b" passes an "Author A" grant
// and the filesystem join then collapses the ".." to reach the out-of-scope
// file. The scope gate, the eventual SafeJoin, and every persisted path key
// (share rules, folder overrides, enrichment, progress) must all see the same
// canonical form, so every user-supplied rel path goes through here before it
// is checked or stored.
func CleanRelPath(p string) string {
	return strings.Trim(path.Clean("/"+filepath.ToSlash(p)), "/")
}

// pathAllowedBy reports whether p is granted by rule path rp (segment-boundary
// prefix match; "" grants everything).
func pathAllowedBy(rp, p string) bool {
	return rp == "" || p == rp || strings.HasPrefix(p, rp+"/")
}

// Allows reports whether the scope grants access to path p.
func (s Scope) Allows(p string) bool {
	if s.AllowAll {
		return true
	}
	for _, rp := range s.Paths {
		if pathAllowedBy(rp, p) {
			return true
		}
	}
	return false
}

// ScopesAllow reports whether one of scopes (a user's per-library scopes, as
// UserScopes returns them) grants ref: the Go twin of scopesFilterSQL.
func ScopesAllow(scopes []Scope, ref Ref) bool {
	for _, s := range scopes {
		if s.LibraryID == ref.LibraryID && s.Allows(ref.Path) {
			return true
		}
	}
	return false
}

// VisibleInBrowse reports whether p should appear when browsing the filtered
// filesystem tree: it is granted (under/equal a rule) OR an ancestor of a rule
// (so the user can navigate toward granted content).
func (s Scope) VisibleInBrowse(p string) bool {
	if s.AllowAll {
		return true
	}
	for _, rp := range s.Paths {
		if pathAllowedBy(rp, p) || strings.HasPrefix(rp, p+"/") {
			return true
		}
	}
	return false
}

// pathFilterSQL builds a WHERE fragment (plus args) restricting col to the
// scope. Returns ("1", nil) for AllowAll, and ("0", nil) for an empty scope.
func pathFilterSQL(col string, s Scope) (string, []any) {
	if s.AllowAll {
		return "1", nil
	}
	if len(s.Paths) == 0 {
		return "0", nil
	}
	var conds []string
	var args []any
	for _, p := range s.Paths {
		// The exact-match arm binds p literally; the subtree arm is the byte range
		// of the paths that start with p+"/" ('0' is the byte after '/'), so it is
		// the literal, case-sensitive prefix of the authoritative Go gate
		// (pathAllowedBy). LIKE was neither: it ignores ASCII case, so a grant on
		// "Saga" reached a sibling "saga/..." on a case-sensitive filesystem, and
		// its wildcards needed escaping.
		conds = append(conds, "("+col+" = ? OR ("+col+" >= ? AND "+col+" < ?))")
		args = append(args, p, p+"/", p+"0")
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

// escapeLike escapes the SQLite LIKE wildcards ('%', '_') and the backslash
// escape character so a value can be used as a literal prefix in a
// `LIKE ? ESCAPE '\'` pattern.
func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// scopesFilterSQL builds a WHERE fragment (plus args) restricting rows to the
// caller's per-library scopes: libCol must equal a scope's library and pathCol
// must be granted by that scope (via pathFilterSQL). Returns ("0", nil) when the
// caller has no scopes (no access). Used to scope the cross-library listing
// endpoints (progress/history) at the query layer, so LIMIT applies to accessible
// rows and access control stays out of the transport layer.
func scopesFilterSQL(libCol, pathCol string, scopes []Scope) (string, []any) {
	if len(scopes) == 0 {
		return "0", nil
	}
	var ors []string
	var args []any
	for _, s := range scopes {
		frag, fargs := pathFilterSQL(pathCol, s)
		ors = append(ors, "("+libCol+" = ? AND "+frag+")")
		args = append(args, s.LibraryID)
		args = append(args, fargs...)
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// UserScope returns a user's effective scope within one library. Admins get
// AllowAll. A non-admin with no grants gets an empty scope (no access).
func (c *Catalog) UserScope(ctx context.Context, userID, libraryID int64, isAdmin bool) (Scope, error) {
	if isAdmin {
		return Scope{LibraryID: libraryID, AllowAll: true}, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT sp.path FROM share_paths sp
		   JOIN user_share_access usa ON usa.share_id = sp.share_id
		  WHERE usa.user_id = ? AND sp.library_id = ?`, userID, libraryID)
	if err != nil {
		return Scope{}, err
	}
	defer rows.Close()
	scope := Scope{LibraryID: libraryID}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return Scope{}, err
		}
		if p == "" {
			scope.AllowAll = true
		} else {
			scope.Paths = append(scope.Paths, p)
		}
	}
	return scope, rows.Err()
}

// UserScopes returns the user's scope for every library they can reach (admins:
// every library, AllowAll). Used by cross-library search.
func (c *Catalog) UserScopes(ctx context.Context, userID int64, isAdmin bool) ([]Scope, error) {
	if isAdmin {
		libs, err := c.ListLibraries(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Scope, len(libs))
		for i, l := range libs {
			out[i] = Scope{LibraryID: l.ID, AllowAll: true}
		}
		return out, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT sp.library_id, sp.path FROM share_paths sp
		   JOIN user_share_access usa ON usa.share_id = sp.share_id
		  WHERE usa.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byLib := map[int64]*Scope{}
	for rows.Next() {
		var libID int64
		var p string
		if err := rows.Scan(&libID, &p); err != nil {
			return nil, err
		}
		s := byLib[libID]
		if s == nil {
			s = &Scope{LibraryID: libID}
			byLib[libID] = s
		}
		if p == "" {
			s.AllowAll = true
		} else {
			s.Paths = append(s.Paths, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Scope, 0, len(byLib))
	for _, s := range byLib {
		out = append(out, *s)
	}
	return out, nil
}

// UserShares returns the shares granted to a user (with their path rules),
// ordered by name. Used by the admin console to show what a user can access.
func (c *Catalog) UserShares(ctx context.Context, userID int64) ([]Share, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.description, s.read_only, s.whole_library_id
		   FROM shares s
		   JOIN user_share_access usa ON usa.share_id = s.id
		  WHERE usa.user_id = ? ORDER BY s.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Paths, err = c.ListSharePaths(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AccessibleLibraries returns libraries the user can reach (admins: all).
func (c *Catalog) AccessibleLibraries(ctx context.Context, userID int64, isAdmin bool) ([]Library, error) {
	if isAdmin {
		return c.ListLibraries(ctx)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT DISTINCT l.id, l.name, l.root, l.default_view, l.sort_order, l.scan_schedule, l.ignore_patterns,
		        l.metadata_source
		   FROM libraries l
		   JOIN share_paths sp ON sp.library_id = l.id
		   JOIN user_share_access usa ON usa.share_id = sp.share_id
		  WHERE usa.user_id = ? ORDER BY l.sort_order, l.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLibraries(rows)
}

// --- Share CRUD + membership + grants ---

// CreateShare creates a share together with any path rules in s.Paths, all in
// one transaction: either the share row and every rule land, or nothing does.
// This keeps the share-with-rules creation atomic in the catalog instead of
// leaving the transport layer to compensate with a manual rollback delete (which
// can itself fail, e.g. on a cancelled request context).
func (c *Catalog) CreateShare(ctx context.Context, s Share) (*Share, error) {
	err := c.db.WithTx(ctx, "CreateShare", func(tx *sql.Tx) error {
		ro := 0
		if s.ReadOnly {
			ro = 1
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO shares(name, description, read_only, created_at) VALUES(?,?,?,?)`,
			s.Name, s.Description, ro, c.ts())
		if err != nil {
			if store.IsUniqueViolation(err) {
				return ErrNameTaken
			}
			return err
		}
		s.ID, _ = res.LastInsertId()
		for _, rule := range s.Paths {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO share_paths(share_id, library_id, path) VALUES(?,?,?)`,
				s.ID, rule.LibraryID, CleanRelPath(rule.Path)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func scanShare(row interface{ Scan(...any) error }) (*Share, error) {
	var s Share
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &s.ReadOnly, &s.WholeLibraryID); err != nil {
		return nil, err
	}
	return &s, nil
}

// ShareName is a share's name alone (ErrNotFound when there is none), without
// GetShare's rules.
func (c *Catalog) ShareName(ctx context.Context, id int64) (string, error) {
	var name string
	err := c.db.QueryRowContext(ctx, `SELECT name FROM shares WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// GetShare returns a share including its path rules.
func (c *Catalog) GetShare(ctx context.Context, id int64) (*Share, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT id, name, description, read_only, whole_library_id FROM shares WHERE id = ?`, id)
	s, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.Paths, err = c.ListSharePaths(ctx, id); err != nil {
		return nil, err
	}
	return s, nil
}

// ListShares returns all shares (with their paths).
func (c *Catalog) ListShares(ctx context.Context) ([]Share, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, description, read_only, whole_library_id FROM shares ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Paths, err = c.ListSharePaths(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ShareMembers maps each share id to the ids of the users it is granted to (the
// console's "People with this share"). Shares granted to nobody are absent.
func (c *Catalog) ShareMembers(ctx context.Context) (map[int64][]int64, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT share_id, user_id FROM user_share_access ORDER BY share_id, user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var shareID, userID int64
		if err := rows.Scan(&shareID, &userID); err != nil {
			return nil, err
		}
		out[shareID] = append(out[shareID], userID)
	}
	return out, rows.Err()
}

// ShareUpdate is a partial share patch. Each field is a pointer so an omitted
// field is left unchanged (a PATCH that sends only {"name":...} must not wipe
// the description or read_only flag).
type ShareUpdate struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	ReadOnly    *bool   `json:"read_only"`
}

// UpdateShare patches a share (see ShareUpdate for the omitted-field
// semantics). A nil name, or an explicit empty name, keeps the existing name.
func (c *Catalog) UpdateShare(ctx context.Context, id int64, in ShareUpdate) (*Share, error) {
	existing, err := c.GetShare(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil && *in.Name != "" {
		existing.Name = *in.Name
	}
	if in.Description != nil {
		existing.Description = *in.Description
	}
	if in.ReadOnly != nil {
		existing.ReadOnly = *in.ReadOnly
	}
	ro := 0
	if existing.ReadOnly {
		ro = 1
	}
	if _, err := c.db.ExecContext(ctx,
		`UPDATE shares SET name = ?, description = ?, read_only = ? WHERE id = ?`,
		existing.Name, existing.Description, ro, id); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	return existing, nil
}

// DeleteShare removes a share (paths + grants cascade).
func (c *Catalog) DeleteShare(ctx context.Context, id int64) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, id)
	return err
}

// AddSharePath adds a path rule to a share. The path is canonicalized on write
// (a non-canonical rule would never prefix-match a request path, silently
// granting nothing).
func (c *Catalog) AddSharePath(ctx context.Context, shareID int64, rule PathRule) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO share_paths(share_id, library_id, path) VALUES(?,?,?)`,
		shareID, rule.LibraryID, CleanRelPath(rule.Path))
	return err
}

// AddSharePaths adds several path rules to a share in one transaction (adding a
// selection of books to a share): all or none.
func (c *Catalog) AddSharePaths(ctx context.Context, shareID int64, rules []PathRule) error {
	return c.db.WithTx(ctx, "AddSharePaths", func(tx *sql.Tx) error {
		for _, rule := range rules {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO share_paths(share_id, library_id, path) VALUES(?,?,?)`,
				shareID, rule.LibraryID, CleanRelPath(rule.Path)); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveSharePath removes a path rule from a share.
func (c *Catalog) RemoveSharePath(ctx context.Context, shareID int64, rule PathRule) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM share_paths WHERE share_id = ? AND library_id = ? AND path = ?`,
		shareID, rule.LibraryID, CleanRelPath(rule.Path))
	return err
}

// ListSharePaths returns a share's path rules.
func (c *Catalog) ListSharePaths(ctx context.Context, shareID int64) ([]PathRule, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT library_id, path FROM share_paths WHERE share_id = ? ORDER BY library_id, path`, shareID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PathRule
	for rows.Next() {
		var r PathRule
		if err := rows.Scan(&r.LibraryID, &r.Path); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GrantShare grants a share to a user.
func (c *Catalog) GrantShare(ctx context.Context, userID, shareID int64) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO user_share_access(user_id, share_id) VALUES(?,?)`, userID, shareID)
	return err
}

// RevokeShare removes a user's grant of a share.
func (c *Catalog) RevokeShare(ctx context.Context, userID, shareID int64) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM user_share_access WHERE user_id = ? AND share_id = ?`, userID, shareID)
	return err
}

// GrantWholeLibrary is convenience sugar: it ensures a share named after the
// library that grants its whole tree ("" rule) exists, and grants it to the
// user. This preserves the simple "give a user a whole library" workflow on top
// of the share model.
func (c *Catalog) GrantWholeLibrary(ctx context.Context, userID, libraryID int64) error {
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil {
		return err
	}
	name := "Library: " + lib.Name
	share, err := c.wholeLibraryShare(ctx, libraryID, name)
	if errors.Is(err, ErrNotFound) {
		share, err = c.CreateShare(ctx, Share{Name: name, Description: "Whole library"})
		if errors.Is(err, ErrNameTaken) {
			// Another library's grant already has this name (that library was
			// renamed and this one took its old name). The name is internal -
			// clients show the library's own - so tell them apart by id.
			share, err = c.CreateShare(ctx, Share{
				Name: fmt.Sprintf("%s (%d)", name, libraryID), Description: "Whole library",
			})
		}
	}
	if err != nil {
		return err
	}
	// Always ensure the whole-library rule exists, not just on creation: a
	// same-named library that was deleted and recreated cascade-deletes the old
	// rule (share_paths FK) while the share row survives, leaving a rule-less
	// share that grants nothing. AddSharePath is INSERT OR IGNORE, so re-running
	// is cheap and self-heals such orphaned shares.
	if err := c.AddSharePath(ctx, share.ID, PathRule{LibraryID: libraryID, Path: ""}); err != nil {
		return err
	}
	// Mark it as this library's grant (idempotent; also heals a share made
	// before the column existed that the migration couldn't match).
	if _, err := c.db.ExecContext(ctx,
		`UPDATE shares SET whole_library_id = ? WHERE id = ?`, libraryID, share.ID); err != nil {
		return err
	}
	return c.GrantShare(ctx, userID, share.ID)
}

// wholeLibraryShare finds the share that grants libraryID whole: the one marked
// with it (which survives a library rename), else an unmarked share named after
// it (made before whole_library_id existed). A same-named share marked for
// another library is never reused - adding this library's rule to it would hand
// this library to everyone holding that one.
func (c *Catalog) wholeLibraryShare(ctx context.Context, libraryID int64, name string) (*Share, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		`SELECT id FROM shares WHERE whole_library_id = ? ORDER BY id LIMIT 1`, libraryID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = c.db.QueryRowContext(ctx,
			`SELECT id FROM shares WHERE name = ? AND whole_library_id IS NULL`, name).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c.GetShare(ctx, id)
}

func (c *Catalog) shareByName(ctx context.Context, name string) (*Share, error) {
	var id int64
	err := c.db.QueryRowContext(ctx, `SELECT id FROM shares WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c.GetShare(ctx, id)
}
