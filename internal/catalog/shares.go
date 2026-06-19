package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Sharing is filesystem-based: a Share is a named set of path rules, where each
// rule's Path is any node in a library's tree ("" = whole library, an author or
// series folder = subtree, a book = single item). Users are granted shares; a
// user's access to a path is the union of all their granted rules.

// Share is a named, grantable bundle of path rules.
type Share struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ReadOnly    bool       `json:"read_only"`
	Paths       []PathRule `json:"paths,omitempty"`
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
		conds = append(conds, "("+col+" = ? OR "+col+" LIKE ? || '/%')")
		args = append(args, p, p)
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
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
		`SELECT s.id, s.name, s.description, s.read_only
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
		`SELECT DISTINCT l.id, l.name, l.root, l.layout, l.default_view
		   FROM libraries l
		   JOIN share_paths sp ON sp.library_id = l.id
		   JOIN user_share_access usa ON usa.share_id = sp.share_id
		  WHERE usa.user_id = ? ORDER BY l.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLibraries(rows)
}

// --- Share CRUD + membership + grants ---

// CreateShare creates a share (without paths) and returns it.
func (c *Catalog) CreateShare(ctx context.Context, s Share) (*Share, error) {
	ro := 1
	if !s.ReadOnly {
		ro = 0
	}
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO shares(name, description, read_only, created_at) VALUES(?,?,?,?)`,
		s.Name, s.Description, ro, c.ts())
	if err != nil {
		return nil, err
	}
	s.ID, _ = res.LastInsertId()
	return &s, nil
}

func scanShare(row interface{ Scan(...any) error }) (*Share, error) {
	var s Share
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &s.ReadOnly); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetShare returns a share including its path rules.
func (c *Catalog) GetShare(ctx context.Context, id int64) (*Share, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT id, name, description, read_only FROM shares WHERE id = ?`, id)
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
		`SELECT id, name, description, read_only FROM shares ORDER BY name`)
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

// UpdateShare patches name/description/read_only (non-empty name only).
func (c *Catalog) UpdateShare(ctx context.Context, id int64, in Share) (*Share, error) {
	existing, err := c.GetShare(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		existing.Name = in.Name
	}
	existing.Description = in.Description
	existing.ReadOnly = in.ReadOnly
	ro := 0
	if existing.ReadOnly {
		ro = 1
	}
	if _, err := c.db.ExecContext(ctx,
		`UPDATE shares SET name = ?, description = ?, read_only = ? WHERE id = ?`,
		existing.Name, existing.Description, ro, id); err != nil {
		return nil, err
	}
	return existing, nil
}

// DeleteShare removes a share (paths + grants cascade).
func (c *Catalog) DeleteShare(ctx context.Context, id int64) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, id)
	return err
}

// AddSharePath adds a path rule to a share.
func (c *Catalog) AddSharePath(ctx context.Context, shareID int64, rule PathRule) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO share_paths(share_id, library_id, path) VALUES(?,?,?)`,
		shareID, rule.LibraryID, rule.Path)
	return err
}

// RemoveSharePath removes a path rule from a share.
func (c *Catalog) RemoveSharePath(ctx context.Context, shareID int64, rule PathRule) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM share_paths WHERE share_id = ? AND library_id = ? AND path = ?`,
		shareID, rule.LibraryID, rule.Path)
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
	share, err := c.shareByName(ctx, name)
	if errors.Is(err, ErrNotFound) {
		share, err = c.CreateShare(ctx, Share{Name: name, Description: "Whole library", ReadOnly: false})
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
	return c.GrantShare(ctx, userID, share.ID)
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
