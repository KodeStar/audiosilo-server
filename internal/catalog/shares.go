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
		return pathsJSONFilterSQL(col, s.Paths)
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
// A path that isn't valid UTF-8 can't ride in the list (encoding/json would
// replace the bad bytes, naming another path), so it keeps an OR arm of its own
// beside it: one such path never sends the whole list to the OR form, whose
// arms pass SQLite's expression depth at about a thousand.
func pathsJSONFilterSQL(col string, paths []string) (string, []any) {
	list := make([]string, 0, len(paths))
	var odd []string
	for _, p := range paths {
		if utf8.ValidString(p) {
			list = append(list, p)
		} else {
			odd = append(odd, p)
		}
	}
	js, err := json.Marshal(list) // valid UTF-8 strings only: it can't fail
	if err != nil || len(list) == 0 {
		return pathsORFilterSQL(col, paths)
	}
	// The row's path is split on its bytes (a BLOB): on TEXT, instr and substr
	// count a stray continuation byte (a path that isn't UTF-8) differently, so
	// "Wait for it\x85/1.mp3" got the false ancestor "Wait for it".
	b := "CAST(" + col + " AS BLOB)"
	frag := "EXISTS(WITH RECURSIVE anc(p, rest) AS (" +
		"SELECT " + col + ", NULL UNION ALL " +
		"SELECT substr(" + b + ", 1, instr(" + b + ", X'2F') - 1), substr(" + b + ", instr(" + b + ", X'2F') + 1) WHERE instr(" + b + ", X'2F') > 0 UNION ALL " +
		"SELECT p || '/' || substr(rest, 1, instr(rest, X'2F') - 1), substr(rest, instr(rest, X'2F') + 1) FROM anc WHERE instr(rest, X'2F') > 0) " +
		"SELECT 1 FROM anc WHERE CAST(p AS TEXT) IN (SELECT value FROM json_each(?)))"
	args := []any{string(js)}
	if len(odd) > 0 {
		oddFrag, oddArgs := pathsORFilterSQL(col, odd)
		frag, args = "("+frag+" OR "+oddFrag+")", append(args, oddArgs...)
	}
	return frag, args
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

// Caller is the person access is computed for: their id, whether they are an
// admin and, for a household member, their member rule. The transport builds it
// from the request's user (api.callerOf), whose token lookup read the rule in
// its own statement, so a request's scopes cost no statement for the rule;
// UserScope, UserScopes and AccessibleLibraries read the rule themselves
// (memberRuleOf) for a caller known only by id.
type Caller struct {
	ID    int64
	Admin bool
	// Podcasts is the authenticated client gate, never inferred from Admin.
	Podcasts bool
	// Member is a household member's rule (members.go), nil for anyone else.
	Member *MemberRule
}

// UserScope returns a user's effective scope within one library. Admins get
// AllowAll for books; background callers exclude podcasts. A non-admin with no
// grants gets an empty scope (no access). A
// household member also gets what their owner gives them (the member rule,
// memberScopes). It reads the rule; CallerScope is it for a Caller.
func (c *Catalog) UserScope(ctx context.Context, userID, libraryID int64, isAdmin bool) (Scope, error) {
	who, err := c.callerByID(ctx, userID, isAdmin)
	if err != nil {
		return Scope{}, err
	}
	return c.CallerScope(ctx, who, libraryID)
}

// UserScopes returns the user's scope for every library they can reach (admins:
// every library, AllowAll), with a household member's derived access (the
// member rule). Used by cross-library search and every cross-library list. It
// reads the rule; CallerScopes is it for a Caller.
func (c *Catalog) UserScopes(ctx context.Context, userID int64, isAdmin bool) ([]Scope, error) {
	who, err := c.callerByID(ctx, userID, isAdmin)
	if err != nil {
		return nil, err
	}
	return c.CallerScopes(ctx, who)
}

// AccessibleLibraries returns libraries the user can reach (admins: all; a
// household member, the libraries their own and derived access reach). It reads
// the rule; CallerLibraries is it for a Caller.
func (c *Catalog) AccessibleLibraries(ctx context.Context, userID int64, isAdmin bool) ([]Library, error) {
	who, err := c.callerByID(ctx, userID, isAdmin)
	if err != nil {
		return nil, err
	}
	return c.CallerLibraries(ctx, who)
}

// callerByID is userID as a Caller, with their member rule read (none for an
// admin, whose scopes it can't change).
func (c *Catalog) callerByID(ctx context.Context, userID int64, isAdmin bool) (Caller, error) {
	who := Caller{ID: userID, Admin: isAdmin}
	if isAdmin {
		return who, nil
	}
	var err error
	who.Member, err = c.memberRuleOf(ctx, userID)
	return who, err
}

// CallerScope is UserScope for who.
func (c *Catalog) CallerScope(ctx context.Context, who Caller, libraryID int64) (Scope, error) {
	lib, err := c.GetLibrary(ctx, libraryID)
	if errors.Is(err, ErrNotFound) {
		return Scope{LibraryID: libraryID, AllowAll: who.Admin}, nil
	}
	if err != nil {
		return Scope{}, err
	}
	if lib.Kind == KindPodcasts && !who.Podcasts {
		return Scope{LibraryID: libraryID}, nil
	}
	scope := Scope{LibraryID: libraryID, AllowAll: who.Admin}
	if !who.Admin {
		scopes, err := c.userScopes(ctx, who, libraryID)
		if err != nil {
			return Scope{}, err
		}
		if s := scopes[libraryID]; s != nil {
			scope = *s
		}
	}
	if lib.Kind == KindPodcasts {
		return c.privatePodcastScope(ctx, who.ID, scope)
	}
	return scope, nil
}

// CallerScopes filters the old-client gate before the caller's private shows.
func (c *Catalog) CallerScopes(ctx context.Context, who Caller) ([]Scope, error) {
	var scopes []Scope
	var err error
	if who.Admin {
		scopes, err = c.allLibraryScopes(ctx)
	} else {
		var byLib map[int64]*Scope
		byLib, err = c.userScopes(ctx, who, 0)
		scopes = flattenScopes(byLib)
	}
	if err != nil {
		return nil, err
	}
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	kind := map[int64]string{}
	for _, l := range libs {
		kind[l.ID] = l.Kind
	}
	out := []Scope{}
	for _, s := range scopes {
		if kind[s.LibraryID] == KindPodcasts {
			if !who.Podcasts {
				continue
			}
			s, err = c.privatePodcastScope(ctx, who.ID, s)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// GatedScopes is who's scopes for a whole-list replace, from one read of their
// access: scopes is CallerScopes, and keep is what the old-client gate alone
// hides from who, their scopes in podcast libraries as a client that sees
// podcasts would get them (shares, household rules and private shows applied;
// nil when who already sees podcasts). A replace from such a caller keeps the
// stored entries keep allows (SetQueue, SetCollectionItems), so an old player
// reordering a list can't delete podcast entries it was never shown. Entries
// outside them (a revoked share, an unfollowed private show) get no such
// protection.
func (c *Catalog) GatedScopes(ctx context.Context, who Caller) (scopes, keep []Scope, err error) {
	if who.Podcasts {
		scopes, err = c.CallerScopes(ctx, who)
		return scopes, nil, err
	}
	who.Podcasts = true
	all, err := c.CallerScopes(ctx, who)
	if err != nil {
		return nil, nil, err
	}
	podcasts, err := c.podcastLibraries(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The gate changes nothing outside podcast libraries, so the rest is
	// exactly CallerScopes for who as they are.
	scopes, keep = []Scope{}, []Scope{}
	for _, s := range all {
		if podcasts[s.LibraryID] {
			keep = append(keep, s)
		} else {
			scopes = append(scopes, s)
		}
	}
	return scopes, keep, nil
}

// podcastLibraries is the set of podcast library ids.
func (c *Catalog) podcastLibraries(ctx context.Context) (map[int64]bool, error) {
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	podcasts := map[int64]bool{}
	for _, l := range libs {
		podcasts[l.ID] = l.Kind == KindPodcasts
	}
	return podcasts, nil
}

// CallerLibraries applies the client gate but keeps library grants independent
// of follows: an empty podcast library is still a destination for following.
func (c *Catalog) CallerLibraries(ctx context.Context, who Caller) ([]Library, error) {
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var scopes map[int64]*Scope
	if !who.Admin {
		scopes, err = c.userScopes(ctx, who, 0)
		if err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(libs, func(l Library) bool {
		if l.Kind == KindPodcasts && !who.Podcasts {
			return true
		}
		if who.Admin {
			return false
		}
		s := scopes[l.ID]
		return s == nil || !s.AllowAll && len(s.Paths) == 0
	}), nil
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
func (c *Catalog) userScopes(ctx context.Context, who Caller, libraryID int64) (map[int64]*Scope, error) {
	own, err := c.grantScopes(ctx, who.ID, libraryID)
	if err != nil || who.Member == nil {
		return own, err
	}
	derived, err := c.memberScopes(ctx, who.ID, libraryID, who.Member)
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
	grants, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *Ref) error {
		return rows.Scan(&r.LibraryID, &r.Path)
	}, `SELECT DISTINCT sp.library_id, sp.path FROM share_paths sp
	      JOIN user_share_access usa ON usa.share_id = sp.share_id
	     WHERE usa.user_id = ?1 AND (?2 = 0 OR sp.library_id = ?2)`, userID, libraryID)
	if err != nil {
		return nil, err
	}
	byLib := map[int64]*Scope{}
	for _, g := range grants {
		s := byLib[g.LibraryID]
		if s == nil {
			s = &Scope{LibraryID: g.LibraryID}
			byLib[g.LibraryID] = s
		}
		if g.Path == "" {
			s.AllowAll = true
		} else {
			s.Paths = append(s.Paths, g.Path)
		}
	}
	for _, s := range byLib {
		if s.AllowAll {
			s.Paths = nil
		}
	}
	return byLib, nil
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
	seen := make(map[string]bool, len(cur.Paths))
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
		share, err = c.createWholeLibraryShare(ctx, libraryID, name)
	}
	if err != nil {
		return err
	}
	// Always ensure the whole-library rule exists, not just on creation: a
	// reused share may lack it (an unheld one an interrupted grant of an older
	// version left; a new one is made whole in one transaction).
	// AddSharePath is INSERT OR IGNORE, so re-running is cheap.
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

// wholeLibraryShareDescription is the description GrantWholeLibrary has always
// given the share it creates.
const wholeLibraryShareDescription = "Whole library"

// wholeLibraryIDName is the name of libraryID's grant share when "Library:
// <name>" (name) is another share's.
func wholeLibraryIDName(name string, libraryID int64) string {
	return fmt.Sprintf("%s (%d)", name, libraryID)
}

// maxGrantNames bounds the names createWholeLibraryShare tries.
const maxGrantNames = 20

// grantName is the i-th name createWholeLibraryShare tries for libraryID's
// grant share: "Library: <name>", then "Library: <name> (<id>)", then that
// numbered from 2.
func grantName(name string, libraryID int64, i int) string {
	switch i {
	case 0:
		return name
	case 1:
		return wholeLibraryIDName(name, libraryID)
	}
	return fmt.Sprintf("%s %d", wholeLibraryIDName(name, libraryID), i)
}

// createWholeLibraryShare makes libraryID's grant share in one transaction:
// named by the first of grantName's names no share has, with the library's ""
// rule and marked as its grant, so no interrupted grant leaves a share without
// its rule. The name is internal (clients show the library's own), so another
// share holding one (another library's grant after a rename, an admin's own
// share, a grant since narrowed, which is never adopted) only moves it along
// (server#230). ErrNameTaken only when every name is taken.
func (c *Catalog) createWholeLibraryShare(ctx context.Context, libraryID int64, name string) (*Share, error) {
	s := &Share{Description: wholeLibraryShareDescription, Paths: []PathRule{{LibraryID: libraryID, Path: ""}},
		WholeLibraryID: &libraryID}
	err := c.db.WithTx(ctx, "createWholeLibraryShare", func(tx *sql.Tx) error {
		for i := range maxGrantNames {
			// A refused INSERT ends only its statement in SQLite, not the
			// transaction, so the next name is tried in the same one.
			res, err := tx.ExecContext(ctx,
				`INSERT INTO shares(name, description, read_only, created_at, whole_library_id) VALUES(?,?,0,?,?)`,
				grantName(name, libraryID, i), s.Description, c.ts(), libraryID)
			if store.IsUniqueViolation(err) {
				continue
			}
			if err != nil {
				return err
			}
			s.ID, _ = res.LastInsertId()
			s.Name = grantName(name, libraryID, i)
			_, err = tx.ExecContext(ctx, `INSERT INTO share_paths(share_id, library_id, path) VALUES(?,?,'')`, s.ID, libraryID)
			return err
		}
		return ErrNameTaken
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// wholeLibraryShare finds the share that grants libraryID whole: the one marked
// with it (which survives a library rename), else an unmarked one with the
// description GrantWholeLibrary gives it, named "Library: <name>" (made before
// whole_library_id existed) or "Library: <name> (<id>)" (one an interrupted
// grant created and couldn't mark). Either way it qualifies only while its sole
// rule is this library's "", or it has no rule and no holder (one an interrupted
// grant left). GrantWholeLibrary adds the "" rule and hands the share out, so
// reusing anything else - an admin's own share with the name (server#223), a
// grant since narrowed or given another library's folders, a rule-less share
// someone holds, or a same-named share marked for another library - would widen
// its holders or hand their folders to the new one. The caller then creates a
// share of its own.
func (c *Catalog) wholeLibraryShare(ctx context.Context, libraryID int64, name string) (*Share, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		`SELECT s.id FROM shares s
		  WHERE (s.whole_library_id = ?1
		         OR (s.whole_library_id IS NULL AND s.name IN (?2, ?3) AND s.description = ?4))
		    AND NOT EXISTS (SELECT 1 FROM share_paths sp
		                     WHERE sp.share_id = s.id AND (sp.library_id <> ?1 OR sp.path <> ''))
		    AND (EXISTS (SELECT 1 FROM share_paths sp WHERE sp.share_id = s.id)
		         OR NOT EXISTS (SELECT 1 FROM user_share_access ua WHERE ua.share_id = s.id))
		  ORDER BY s.whole_library_id IS NULL, s.id LIMIT 1`,
		libraryID, name, wholeLibraryIDName(name, libraryID), wholeLibraryShareDescription).Scan(&id)
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
