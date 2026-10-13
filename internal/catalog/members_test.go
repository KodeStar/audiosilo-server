package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The household member rule (members.go, security-critical): every allowed case
// has its denied twin. Accounts are written straight into users (catalog never
// imports auth, which keeps the member columns in production).

type memberFixture struct {
	c          *Catalog
	ctx        context.Context
	libA, libB int64
}

func newMemberFixture(t testing.TB) *memberFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &memberFixture{c: c, ctx: ctx}
	a, err := c.CreateLibrary(ctx, Library{Name: "A", Root: "/tmp/a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateLibrary(ctx, Library{Name: "B", Root: "/tmp/b"})
	if err != nil {
		t.Fatal(err)
	}
	f.libA, f.libB = a.ID, b.ID
	for _, p := range []string{"Author/Book", "Author/Book 2", "Author/Book/Nested", "Other/X"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: f.libA, RelPath: p, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: f.libB, RelPath: "B/One", Title: "One"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// user inserts an account: role, and (owner > 0) a member of owner with kind and
// access.
func (f *memberFixture) user(t testing.TB, name, role string, owner int64, access string) int64 {
	t.Helper()
	var managed, kind, acc any
	if owner > 0 {
		managed, kind, acc = owner, "kid", access
	}
	res, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at, managed_by, member_kind, member_access)
		 VALUES(?, '', ?, 't', 't', ?, ?, ?)`, name, role, managed, kind, acc)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *memberFixture) exec(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}

// grant gives userID the path of lib through a share of its own.
func (f *memberFixture) grant(t testing.TB, userID, lib int64, path string) int64 {
	t.Helper()
	s, err := f.c.CreateShare(f.ctx, Share{Name: fmt.Sprintf("u%d-l%d-%s", userID, lib, path),
		Paths: []PathRule{{LibraryID: lib, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.GrantShare(f.ctx, userID, s.ID); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// collection makes owner's collection holding items (lib A) shared with viewers.
func (f *memberFixture) collection(t testing.TB, owner int64, items []string, viewers ...int64) int64 {
	t.Helper()
	col, err := f.c.CreateCollection(f.ctx, owner, fmt.Sprintf("c%d-%d", owner, len(items)), "")
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range items {
		f.exec(t, `INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,'t')`,
			col.ID, f.libA, p, i)
	}
	for _, v := range viewers {
		f.exec(t, `INSERT INTO collection_shares(collection_id, user_id, created_at) VALUES(?,?,'t')`, col.ID, v)
	}
	return col.ID
}

// allows reports whether userID (not an admin) may reach path in lib, by
// UserScope and by UserScopes (which must agree).
func (f *memberFixture) allows(t *testing.T, userID, lib int64, path string) bool {
	t.Helper()
	s, err := f.c.UserScope(f.ctx, userID, lib, false)
	if err != nil {
		t.Fatal(err)
	}
	all, err := f.c.UserScopes(f.ctx, userID, false)
	if err != nil {
		t.Fatal(err)
	}
	one := s.Allows(CleanRelPath(path))
	if many := ScopesAllow(all, Ref{LibraryID: lib, Path: CleanRelPath(path)}); many != one {
		t.Fatalf("UserScope says %v, UserScopes %v for %q", one, many, path)
	}
	return one
}

func (f *memberFixture) libraries(t *testing.T, userID int64) []int64 {
	t.Helper()
	libs, err := f.c.AccessibleLibraries(f.ctx, userID, false)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, l := range libs {
		ids = append(ids, l.ID)
	}
	return ids
}

func TestMemberRule_All(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	ellis := f.user(t, "ellis", "user", adam, "all")
	bob := f.user(t, "bob", "user", 0, "")
	f.grant(t, bob, f.libB, "")

	// Before the owner has anything, the member has nothing.
	if f.allows(t, ellis, f.libA, "Author/Book") || len(f.libraries(t, ellis)) != 0 {
		t.Fatal("member reaches something its owner can't")
	}
	share := f.grant(t, adam, f.libA, "Author")
	// Allowed: the owner's current access, live.
	if !f.allows(t, ellis, f.libA, "Author/Book 2") || !slices.Equal(f.libraries(t, ellis), []int64{f.libA}) {
		t.Fatal("all member doesn't reach the owner's grant")
	}
	// Denied: outside the owner's grant, and another person's grant.
	if f.allows(t, ellis, f.libA, "Other/X") || f.allows(t, ellis, f.libB, "B/One") {
		t.Fatal("all member reaches beyond the owner")
	}
	// A library granted to the owner afterwards reaches the member at once.
	f.grant(t, adam, f.libB, "")
	if !f.allows(t, ellis, f.libB, "B/One") {
		t.Fatal("a later grant to the owner didn't reach the member")
	}
	// Revoked from the owner: gone for the member at once.
	if err := f.c.RevokeShare(f.ctx, adam, share); err != nil {
		t.Fatal(err)
	}
	if f.allows(t, ellis, f.libA, "Author/Book") || !slices.Equal(f.libraries(t, ellis), []int64{f.libB}) {
		t.Fatal("a grant revoked from the owner still reaches the member")
	}
}

func TestMemberRule_AdminOwner(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	admin := f.user(t, "root", "admin", 0, "")
	all := f.user(t, "all", "user", admin, "all")
	cols := f.user(t, "cols", "user", admin, "collections")
	f.collection(t, admin, []string{"Author/Book"}, cols)

	// Allowed: an admin owner's all member reaches every library (as content).
	if !f.allows(t, all, f.libA, "Other/X") || !f.allows(t, all, f.libB, "B/One") ||
		!slices.Equal(f.libraries(t, all), []int64{f.libA, f.libB}) {
		t.Fatal("admin owner's member doesn't reach every library")
	}
	// The collections member reaches the shared book (the owner reaches it).
	if !f.allows(t, cols, f.libA, "Author/Book") {
		t.Fatal("admin owner's collections member doesn't reach the shared book")
	}
	// Denied: only that book.
	if f.allows(t, cols, f.libA, "Other/X") || f.allows(t, cols, f.libB, "B/One") {
		t.Fatal("admin owner's collections member reaches unshared books")
	}
}

func TestMemberRule_Collections(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	evie := f.user(t, "evie", "user", adam, "collections")
	bob := f.user(t, "bob", "user", 0, "")
	f.grant(t, adam, f.libA, "")
	f.grant(t, bob, f.libA, "")
	f.collection(t, adam, []string{"Author/Book"}, evie)
	f.collection(t, adam, []string{"Other/X"})            // not shared with evie
	f.collection(t, bob, []string{"Author/Book 2"}, evie) // shared by someone else

	// Allowed: the shared book and its own subtree (a folder book's files and
	// what is inside its folder).
	for _, p := range []string{"Author/Book", "Author/Book/01.mp3", "Author/Book/Nested"} {
		if !f.allows(t, evie, f.libA, p) {
			t.Errorf("%q: denied", p)
		}
	}
	if !slices.Equal(f.libraries(t, evie), []int64{f.libA}) {
		t.Errorf("libraries = %v", f.libraries(t, evie))
	}
	// Denied: the segment-boundary sibling, ".." out of the book, the folder above,
	// an unshared collection's book, a book in a collection a non-owner shared.
	for _, p := range []string{"Author/Book 2", "Author/Book 2/01.mp3", "Author/Book/../Book 2", "Author",
		"Other/X", "", "Author/Bookish"} {
		if f.allows(t, evie, f.libA, p) {
			t.Errorf("%q: allowed", p)
		}
	}
	if f.allows(t, evie, f.libB, "B/One") {
		t.Error("another library allowed")
	}
}

func TestMemberRule_OwnerLosesAccess(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	evie := f.user(t, "evie", "user", adam, "collections")
	whole := f.grant(t, adam, f.libA, "")
	f.grant(t, adam, f.libA, "Other")
	f.collection(t, adam, []string{"Author/Book", "Other/X"}, evie)
	if !f.allows(t, evie, f.libA, "Author/Book") || !f.allows(t, evie, f.libA, "Other/X") {
		t.Fatal("shared books not reachable while the owner reaches them")
	}
	// The owner keeps only "Other": the other book drops out for the member.
	if err := f.c.RevokeShare(f.ctx, adam, whole); err != nil {
		t.Fatal(err)
	}
	if f.allows(t, evie, f.libA, "Author/Book") {
		t.Fatal("a book the owner lost still reaches the member")
	}
	if !f.allows(t, evie, f.libA, "Other/X") {
		t.Fatal("a book the owner still reaches dropped out")
	}
}

func TestMemberRule_DisabledOwnerChangesNothing(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	ellis := f.user(t, "ellis", "user", adam, "all")
	evie := f.user(t, "evie", "user", adam, "collections")
	f.grant(t, adam, f.libA, "Author")
	f.collection(t, adam, []string{"Author/Book"}, evie)
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = ?`, adam)
	if !f.allows(t, ellis, f.libA, "Author/Book 2") || !f.allows(t, evie, f.libA, "Author/Book") {
		t.Fatal("disabling the owner changed the members' access")
	}
	if f.allows(t, ellis, f.libA, "Other/X") || f.allows(t, evie, f.libA, "Author/Book 2") {
		t.Fatal("disabling the owner widened the members' access")
	}
}

func TestMemberRule_OwnGrantsUnion(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	evie := f.user(t, "evie", "user", adam, "collections")
	f.grant(t, adam, f.libA, "")
	f.grant(t, evie, f.libB, "")
	f.collection(t, adam, []string{"Author/Book"}, evie)
	if !f.allows(t, evie, f.libB, "B/One") || !f.allows(t, evie, f.libA, "Author/Book") {
		t.Fatal("own grants and derived access aren't a union")
	}
	if f.allows(t, evie, f.libA, "Other/X") {
		t.Fatal("union reaches more than either")
	}
}

// One level only: a member whose owner is (by bad data) a member, or themselves,
// derives nothing; a whole-library ("") item never grants the library.
func TestMemberRule_DerivesNothingFromAMemberOwner(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	f.grant(t, adam, f.libA, "")
	ellis := f.user(t, "ellis", "user", adam, "all")
	zed := f.user(t, "zed", "user", ellis, "all") // a chain auth never makes
	self := f.user(t, "self", "user", 0, "")
	f.grant(t, self, f.libB, "")
	f.exec(t, `UPDATE users SET managed_by = id, member_kind = 'kid', member_access = 'all' WHERE id = ?`, self)
	if !f.allows(t, ellis, f.libA, "Other/X") {
		t.Fatal("ellis lost the owner's access")
	}
	if f.allows(t, zed, f.libA, "Other/X") || len(f.libraries(t, zed)) != 0 {
		t.Fatal("a member derived access from a member-owner")
	}
	if !f.allows(t, self, f.libB, "B/One") || f.allows(t, self, f.libA, "Other/X") {
		t.Fatal("a self-managed row derived something or lost its own grant")
	}

	evie := f.user(t, "evie", "user", adam, "collections")
	f.collection(t, adam, []string{""}, evie)
	if f.allows(t, evie, f.libA, "Other/X") {
		t.Fatal(`an "" item granted the whole library`)
	}
}

// A collections member with thousands of items gets the single-parameter filter
// (pathsJSONFilterSQL) in ListBooks, which returns exactly the rows it may see.
func TestMemberRule_LongCollectionLists(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	adam := f.user(t, "adam", "user", 0, "")
	evie := f.user(t, "evie", "user", adam, "collections")
	f.grant(t, adam, f.libA, "")
	items := []string{"Author/Book"}
	for i := range 1200 {
		items = append(items, fmt.Sprintf("Missing/%04d", i))
	}
	f.collection(t, adam, items, evie)
	s, err := f.c.UserScope(f.ctx, evie, f.libA, false)
	if err != nil || len(s.Paths) != len(items) {
		t.Fatalf("scope = %d paths, %v", len(s.Paths), err)
	}
	page, err := f.c.ListBooks(f.ctx, ListOptions{LibraryID: f.libA, Scope: &s, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range page.Books {
		got = append(got, b.RelPath)
	}
	slices.Sort(got)
	if want := []string{"Author/Book", "Author/Book/Nested"}; !slices.Equal(got, want) {
		t.Fatalf("books = %v, want %v", got, want)
	}
}

// The single-parameter form and the OR form of pathFilterSQL return the same
// rows: the segment boundary, bytes below and above '/', case, and nested paths.
func TestPathFilterForms(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	rows := []string{"Book", "Book 2", "Book-x", "Book.x", "Book/1.mp3", "Book/CD1/1.mp3", "Book0", "Booka", "book/1.mp3",
		"Bookish", "Saga/One", "Saga/One 2", "Saga/One/x", "Ä/b", "Ä b", "z", "Z/y", "Other/Book"}
	for _, p := range rows {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{"Book", "Saga/One", "Ä", "z", "Nope/None"}
	for i := len(paths); i <= jsonPathsAbove+1; i++ {
		paths = append(paths, fmt.Sprintf("Filler/%d", i))
	}
	query := func(frag string, args []any) []string {
		t.Helper()
		got, err := queryRows(ctx, c.db, func(rs *sql.Rows, p *string) error { return rs.Scan(p) },
			`SELECT rel_path FROM books WHERE library_id = ? AND `+frag+` ORDER BY rel_path`, append([]any{lib.ID}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	orFrag, orArgs := pathsORFilterSQL("rel_path", paths)
	jsFrag, jsArgs, ok := pathsJSONFilterSQL("rel_path", paths)
	if !ok || len(jsArgs) != 1 {
		t.Fatalf("json form = %v, %d args", ok, len(jsArgs))
	}
	orRows, jsRows := query(orFrag, orArgs), query(jsFrag, jsArgs)
	want := []string{"Book", "Book/1.mp3", "Book/CD1/1.mp3", "Saga/One", "Saga/One/x", "z", "Ä/b"}
	if !slices.Equal(orRows, want) || !slices.Equal(jsRows, want) {
		t.Fatalf("or = %v\njson = %v\nwant %v", orRows, jsRows, want)
	}
	// pathFilterSQL takes the JSON form above the threshold, the OR form below.
	if frag, _ := pathFilterSQL("rel_path", Scope{Paths: paths}); !strings.HasPrefix(frag, "EXISTS") {
		t.Error("long scope not in the single-parameter form")
	}
	if frag, _ := pathFilterSQL("rel_path", Scope{Paths: paths[:3]}); strings.HasPrefix(frag, "EXISTS") {
		t.Error("short scope in the single-parameter form")
	}
	// A path JSON can't carry exactly falls back to the OR form.
	if _, _, ok := pathsJSONFilterSQL("rel_path", []string{"a\xffb"}); ok {
		t.Error("invalid UTF-8 path took the JSON form")
	}
}

// A non-member's scopes, given as a Caller (the transport's path, whose token
// lookup read the member rule), read only their grants: with the users table
// renamed away they still load, so they cost the one statement they did before
// household members. Denied: the by-id form, which reads the rule from users,
// fails without it.
func TestCallerScopes_NonMemberReadsNoRule(t *testing.T) {
	t.Parallel()
	f := newMemberFixture(t)
	bob := f.user(t, "bob", "user", 0, "")
	f.grant(t, bob, f.libA, "Author")
	who := Caller{ID: bob}
	f.exec(t, `ALTER TABLE users RENAME TO users_away`)
	s, err := f.c.CallerScope(f.ctx, who, f.libA)
	if err != nil || !s.Allows("Author/Book") || s.Allows("Other/X") {
		t.Fatalf("scope = %+v %v", s, err)
	}
	if all, err := f.c.CallerScopes(f.ctx, who); err != nil || len(all) != 1 {
		t.Fatalf("scopes = %+v %v", all, err)
	}
	if _, err := f.c.UserScopes(f.ctx, bob, false); err == nil {
		t.Fatal("the by-id form read no rule")
	}
}
