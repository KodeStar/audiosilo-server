package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/relpath"
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
// root. Security-relevant: relpath.Within is a literal prefix match, so without
// prior normalization "Author A/../Author B/x.m4b" passes an "Author A" grant
// and the filesystem join then collapses the ".." to reach the out-of-scope
// file. The scope gate, the eventual SafeJoin, and every persisted path key
// (share rules, folder overrides, enrichment, progress) must all see the same
// canonical form, so every user-supplied rel path goes through here before it
// is checked or stored. It is relpath.Clean, which auth applies to a cast
// token's path itself (auth never imports catalog).
func CleanRelPath(p string) string {
	return relpath.Clean(p)
}

// Allows reports whether the scope grants access to path p.
func (s Scope) Allows(p string) bool {
	if s.AllowAll {
		return true
	}
	for _, rp := range s.Paths {
		if relpath.Within(rp, p) {
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
		if relpath.Within(rp, p) || strings.HasPrefix(rp, p+"/") {
			return true
		}
	}
	return false
}

// jsonPathsAbove is how many paths a scope may hold before pathFilterSQL binds
// them as one JSON array (json_each) instead of an OR per path: a household
// member's collections can hold thousands of books, and SQLite caps a
// statement's parameters.
const jsonPathsAbove = 32

// pathFilterSQL builds a WHERE fragment (plus args) restricting col to the
// scope. Returns ("1", nil) for AllowAll, and ("0", nil) for an empty scope.
// Above jsonPathsAbove paths it is the single-parameter form (pathsJSONFilterSQL),
// which returns the same rows.
func pathFilterSQL(col string, s Scope) (string, []any) {
	if s.AllowAll {
		return "1", nil
	}
	if len(s.Paths) == 0 {
		return "0", nil
	}
	if len(s.Paths) > jsonPathsAbove {
		if frag, args, ok := pathsJSONFilterSQL(col, s.Paths); ok {
			return frag, args
		}
	}
	return pathsORFilterSQL(col, s.Paths)
}

// pathsORFilterSQL is pathFilterSQL's form with one OR arm per path.
func pathsORFilterSQL(col string, paths []string) (string, []any) {
	var conds []string
	var args []any
	for _, p := range paths {
		// The exact-match arm binds p literally; the subtree arm is the byte range
		// of the paths that start with p+"/" ('0' is the byte after '/'), so it is
		// the literal, case-sensitive prefix of the authoritative Go gate
		// (relpath.Within). LIKE was neither: it ignores ASCII case, so a grant on
		// "Saga" reached a sibling "saga/..." on a case-sensitive filesystem, and
		// its wildcards needed escaping.
		conds = append(conds, "("+col+" = ? OR ("+col+" >= ? AND "+col+" < ?))")
		args = append(args, p, p+"/", p+"0")
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

// pathsJSONFilterSQL is pathFilterSQL's single-parameter form: the paths as one
// JSON array. A row passes when its path, or one of its prefixes ending just
// before a '/' (its ancestors), is one of the paths: exactly the OR form's
// exact-match and byte-range arms (relpath.Within). The ancestors come from the
// row's own path (a recursive CTE, a few rows each) and are looked up in the
// list, which SQLite builds once per statement as an index (an uncorrelated
// IN), so a row costs its depth, not the length of the list: a json_each walk
// per row cost a 1,000-item member about a hundred times an ordinary listener
// (BenchmarkMemberScopes), and 1,000 OR arms pass SQLite's expression depth.
// ok is false when a path isn't valid UTF-8, which JSON can't carry exactly
// (encoding/json would replace the bad bytes, naming another path): the caller
// uses the OR form.
func pathsJSONFilterSQL(col string, paths []string) (string, []any, bool) {
	for _, p := range paths {
		if !utf8.ValidString(p) {
			return "", nil, false
		}
	}
	list, err := json.Marshal(paths)
	if err != nil {
		return "", nil, false
	}
	return "EXISTS(WITH RECURSIVE anc(p, rest) AS (" +
		"SELECT " + col + ", NULL UNION ALL " +
		"SELECT substr(" + col + ", 1, instr(" + col + ", '/') - 1), substr(" + col + ", instr(" + col + ", '/') + 1) WHERE instr(" + col + ", '/') > 0 UNION ALL " +
		"SELECT p || '/' || substr(rest, 1, instr(rest, '/') - 1), substr(rest, instr(rest, '/') + 1) FROM anc WHERE instr(rest, '/') > 0) " +
		"SELECT 1 FROM anc WHERE p IN (SELECT value FROM json_each(?)))", []any{string(list)}, true
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
// AllowAll. A non-admin with no grants gets an empty scope (no access). A
// household member also gets what their owner gives them (the member rule,
// memberScopes).
func (c *Catalog) UserScope(ctx context.Context, userID, libraryID int64, isAdmin bool) (Scope, error) {
	if isAdmin {
		return Scope{LibraryID: libraryID, AllowAll: true}, nil
	}
	scopes, err := c.userScopes(ctx, userID, libraryID)
	if err != nil {
		return Scope{}, err
	}
	if s, ok := scopes[libraryID]; ok {
		return *s, nil
	}
	return Scope{LibraryID: libraryID}, nil
}

// UserScopes returns the user's scope for every library they can reach (admins:
// every library, AllowAll), with a household member's derived access (the
// member rule). Used by cross-library search and every cross-library list.
func (c *Catalog) UserScopes(ctx context.Context, userID int64, isAdmin bool) ([]Scope, error) {
	if isAdmin {
		return c.allLibraryScopes(ctx)
	}
	scopes, err := c.userScopes(ctx, userID, 0)
	if err != nil {
		return nil, err
	}
	return flattenScopes(scopes), nil
}

// allLibraryScopes is every library, AllowAll: an admin's scopes.
func (c *Catalog) allLibraryScopes(ctx context.Context) ([]Scope, error) {
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

// userScopes is a non-admin's scopes by library (in libraryID only, or every
// library for 0): their own grants, plus a household member's derived access.
func (c *Catalog) userScopes(ctx context.Context, userID, libraryID int64) (map[int64]*Scope, error) {
	own, err := c.grantScopes(ctx, userID, libraryID)
	if err != nil {
		return nil, err
	}
	rule, err := c.memberRuleOf(ctx, userID)
	if err != nil || rule == nil {
		return own, err
	}
	derived, err := c.memberScopes(ctx, userID, libraryID, rule)
	if err != nil {
		return nil, err
	}
	for _, s := range derived {
		mergeScope(own, s)
	}
	return own, nil
}

// grantScopes is the scopes a user's own share grants give them, by library (in
// libraryID only, or every library for 0): the admin's grants, nothing derived.
func (c *Catalog) grantScopes(ctx context.Context, userID, libraryID int64) (map[int64]*Scope, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT sp.library_id, sp.path FROM share_paths sp
		   JOIN user_share_access usa ON usa.share_id = sp.share_id
		  WHERE usa.user_id = ?1 AND (?2 = 0 OR sp.library_id = ?2)`, userID, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byLib := map[int64]*Scope{}
	for rows.Next() {
		var s Scope
		var p string
		if err := rows.Scan(&s.LibraryID, &p); err != nil {
			return nil, err
		}
		if p == "" {
			s.AllowAll = true
		} else {
			s.Paths = []string{p}
		}
		mergeScope(byLib, s)
	}
	return byLib, rows.Err()
}

// mergeScope adds s to the scopes by library: the union (AllowAll wins and drops
// the paths; a path already there is not added twice).
func mergeScope(byLib map[int64]*Scope, s Scope) {
	cur := byLib[s.LibraryID]
	if cur == nil {
		cur = &Scope{LibraryID: s.LibraryID}
		byLib[s.LibraryID] = cur
	}
	if cur.AllowAll || s.AllowAll {
		cur.AllowAll, cur.Paths = true, nil
		return
	}
	seen := make(map[string]bool, len(cur.Paths)+len(s.Paths))
	for _, p := range cur.Paths {
		seen[p] = true
	}
	for _, p := range s.Paths {
		if !seen[p] {
			seen[p] = true
			cur.Paths = append(cur.Paths, p)
		}
	}
}

// flattenScopes lists scopes by library id, leaving out any that grant nothing.
func flattenScopes(byLib map[int64]*Scope) []Scope {
	out := make([]Scope, 0, len(byLib))
	for _, s := range byLib {
		if s.AllowAll || len(s.Paths) > 0 {
			out = append(out, *s)
		}
	}
	slices.SortFunc(out, func(a, b Scope) int { return cmp.Compare(a.LibraryID, b.LibraryID) })
	return out
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

// AccessibleLibraries returns libraries the user can reach (admins: all; a
// household member, the libraries their own and derived access reach).
func (c *Catalog) AccessibleLibraries(ctx context.Context, userID int64, isAdmin bool) ([]Library, error) {
	if isAdmin {
		return c.ListLibraries(ctx)
	}
	rule, err := c.memberRuleOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if rule != nil {
		scopes, err := c.userScopes(ctx, userID, 0)
		if err != nil {
			return nil, err
		}
		libs, err := c.ListLibraries(ctx)
		if err != nil {
			return nil, err
		}
		reach := flattenScopes(scopes)
		return slices.DeleteFunc(libs, func(l Library) bool {
			return !slices.ContainsFunc(reach, func(s Scope) bool { return s.LibraryID == l.ID })
		}), nil
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
