package catalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Names are trimmed and must be 1-100 characters without control characters;
// descriptions at most 1000, line breaks allowed.
func TestCollectionNameAndDescription(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		err  bool
	}{
		{"  Road trip  ", "Road trip", false},
		{strings.Repeat("é", 100), strings.Repeat("é", 100), false},
		{strings.Repeat("é", 101), "", true},
		{"   ", "", true},
		{"", "", true},
		{"bad\nname", "", true},
		{"bad\x00name", "", true},
		{"bad\u0085name", "", true},
		{"\xff", "", true},
	} {
		got, err := CleanCollectionName(tc.in)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("CleanCollectionName(%q) = %q, %v", tc.in, got, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("CleanCollectionName(%q) error = %v, want ErrInvalidName", tc.in, err)
		}
	}
	for _, tc := range []struct {
		in  string
		err bool
	}{
		{"", false},
		{strings.Repeat("x", 1000), false},
		{strings.Repeat("x", 1001), true},
		{"two\nlines\tand a tab", false},
		{"bell\a", true},
		{"\xff", true},
	} {
		if _, err := CleanCollectionDescription(tc.in); (err != nil) != tc.err ||
			(err != nil && !errors.Is(err, ErrInvalidDescription)) {
			t.Errorf("CleanCollectionDescription(%.20q) = %v", tc.in, err)
		}
	}
}

// collectionsEnv is a catalog with a library holding two books (Kid/A,
// Adult/X), an owner and a viewer whose access is the Kid folder only, and a
// stranger. The clock moves a second per read of it.
type collectionsEnv struct {
	c                       *Catalog
	lib                     *Library
	owner, viewer, stranger int64
	all, kid                []Scope
}

func newCollectionsEnv(t *testing.T) *collectionsEnv {
	t.Helper()
	c, ctx := newTestCatalog(t)
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"Kid/A", "Adult/X"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	return &collectionsEnv{c: c, lib: lib,
		owner: seedNamedUser(t, c, "olive"), viewer: seedNamedUser(t, c, "vic"), stranger: seedNamedUser(t, c, "sam"),
		all: []Scope{{LibraryID: lib.ID, AllowAll: true}}, kid: []Scope{{LibraryID: lib.ID, Paths: []string{"Kid"}}}}
}

func (e *collectionsEnv) ref(p string) Ref { return Ref{LibraryID: e.lib.ID, Path: p} }

// A viewer reads a shared collection with only the items their own access
// allows (count and preview included), sees no share list, gets ErrNotOwner on
// every write and can leave; a stranger gets ErrNotFound for everything.
func TestCollectionRoles(t *testing.T) {
	e := newCollectionsEnv(t)
	c, ctx := e.c, t.Context()
	col, err := c.CreateCollection(ctx, e.owner, " Bedtime ", "")
	if err != nil {
		t.Fatal(err)
	}
	if col.Name != "Bedtime" || !col.Owned || col.SharedWith == nil || len(*col.SharedWith) != 0 ||
		col.ItemCount != 0 || col.Preview == nil || col.Owner.Username != "olive" {
		t.Fatalf("created = %+v", col)
	}
	for _, p := range []string{"Adult/X", "Kid/A"} {
		if err := c.AddCollectionItem(ctx, col.ID, e.owner, e.ref(p), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetCollectionShares(ctx, col.ID, e.owner, []int64{e.viewer}); err != nil {
		t.Fatal(err)
	}

	// The owner sees both items and who it is shared with.
	got, items, err := c.CollectionDetail(ctx, col.ID, e.owner, e.all)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemCount != 2 || len(items) != 2 || len(got.Preview) != 2 || got.SharedWith == nil ||
		len(*got.SharedWith) != 1 || (*got.SharedWith)[0].Username != "vic" {
		t.Fatalf("owner's view = %+v, %d items", got, len(items))
	}
	// The viewer sees only Kid/A, and no share list.
	got, items, err = c.CollectionDetail(ctx, col.ID, e.viewer, e.kid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owned || got.SharedWith != nil || got.ItemCount != 1 || len(items) != 1 || items[0].Path != "Kid/A" ||
		len(got.Preview) != 1 || got.Preview[0].RelPath != "Kid/A" {
		t.Fatalf("viewer's view = %+v, items %+v", got, items)
	}
	list, err := c.Collections(ctx, e.viewer, e.kid)
	if err != nil || len(list) != 1 || list[0].ItemCount != 1 || list[0].SharedWith != nil {
		t.Fatalf("viewer's list = %+v, %v", list, err)
	}

	// Viewer writes: ErrNotOwner. Stranger: ErrNotFound, for reads too.
	name := "Mine now"
	writes := map[string]func(user int64) error{
		"update":       func(u int64) error { return c.UpdateCollection(ctx, col.ID, u, &name, nil) },
		"add item":     func(u int64) error { return c.AddCollectionItem(ctx, col.ID, u, e.ref("Kid/A"), intp(0)) },
		"set items":    func(u int64) error { return c.SetCollectionItems(ctx, col.ID, u, nil, e.all) },
		"remove item":  func(u int64) error { return c.RemoveCollectionItem(ctx, col.ID, u, e.ref("Kid/A")) },
		"set shares":   func(u int64) error { return c.SetCollectionShares(ctx, col.ID, u, nil) },
		"read":         func(u int64) error { _, err := c.Collection(ctx, col.ID, u, e.all); return err },
		"delete/leave": func(u int64) error { return c.DeleteCollection(ctx, col.ID, u) },
	}
	for what, write := range writes {
		if err := write(e.stranger); !errors.Is(err, ErrNotFound) {
			t.Errorf("stranger %s = %v, want ErrNotFound", what, err)
		}
		if what == "read" || what == "delete/leave" {
			continue
		}
		if err := write(e.viewer); !errors.Is(err, ErrNotOwner) {
			t.Errorf("viewer %s = %v, want ErrNotOwner", what, err)
		}
	}
	if list, _ := c.Collections(ctx, e.stranger, e.all); len(list) != 0 {
		t.Fatalf("stranger lists %+v", list)
	}
	// Nothing changed.
	got, items, _ = c.CollectionDetail(ctx, col.ID, e.owner, e.all)
	if got.Name != "Bedtime" || len(items) != 2 || len(*got.SharedWith) != 1 {
		t.Fatalf("after refused writes = %+v, %d items", got, len(items))
	}

	// The viewer leaves: their share goes, the collection stays.
	if err := c.DeleteCollection(ctx, col.ID, e.viewer); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collection(ctx, col.ID, e.viewer, e.kid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer after leaving = %v, want ErrNotFound", err)
	}
	got, _ = c.Collection(ctx, col.ID, e.owner, e.all)
	if got == nil || len(*got.SharedWith) != 0 {
		t.Fatalf("owner's after the viewer left = %+v", got)
	}
	// The owner deletes it.
	if err := c.DeleteCollection(ctx, col.ID, e.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collection(ctx, col.ID, e.owner, e.all); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v, want ErrNotFound", err)
	}
}

// Owned collections list first, then shared ones, each newest updated_at first;
// a rename, a description change and an item change move updated_at, a no-op
// add does not.
func TestCollectionsOrderAndUpdatedAt(t *testing.T) {
	e := newCollectionsEnv(t)
	c, ctx := e.c, t.Context()
	mk := func(owner int64, name string) *Collection {
		t.Helper()
		col, err := c.CreateCollection(ctx, owner, name, "")
		if err != nil {
			t.Fatal(err)
		}
		return col
	}
	a, b := mk(e.owner, "A"), mk(e.owner, "B")
	s1, s2 := mk(e.stranger, "S1"), mk(e.stranger, "S2")
	for _, s := range []*Collection{s1, s2} {
		if err := c.SetCollectionShares(ctx, s.ID, e.stranger, []int64{e.owner}); err != nil {
			t.Fatal(err)
		}
	}
	names := func() string {
		t.Helper()
		list, err := c.Collections(ctx, e.owner, e.all)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, col := range list {
			out = append(out, col.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "B,A,S2,S1" {
		t.Fatalf("order = %s", got)
	}

	stamp := func(id int64) string {
		t.Helper()
		col, err := c.Collection(ctx, id, e.owner, e.all)
		if err != nil {
			t.Fatal(err)
		}
		return col.UpdatedAt
	}
	before := stamp(a.ID)
	if err := c.AddCollectionItem(ctx, a.ID, e.owner, e.ref("Kid/A"), nil); err != nil {
		t.Fatal(err)
	}
	afterAdd := stamp(a.ID)
	if afterAdd == before {
		t.Fatal("an item change did not move updated_at")
	}
	if got := names(); got != "A,B,S2,S1" {
		t.Fatalf("order after A changed = %s", got)
	}
	if err := c.AddCollectionItem(ctx, a.ID, e.owner, e.ref("Kid/A"), nil); err != nil {
		t.Fatal(err)
	}
	if stamp(a.ID) != afterAdd {
		t.Fatal("a no-op add moved updated_at")
	}
	desc := "Stories"
	if err := c.UpdateCollection(ctx, b.ID, e.owner, nil, &desc); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Collection(ctx, b.ID, e.owner, e.all); got.Description != "Stories" || got.Name != "B" {
		t.Fatalf("after description change = %+v", got)
	}
	if got := names(); got != "B,A,S2,S1" {
		t.Fatalf("order after B changed = %s", got)
	}
	bad := strings.Repeat("x", 101)
	if err := c.UpdateCollection(ctx, b.ID, e.owner, &bad, nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad rename = %v, want ErrInvalidName", err)
	}
}

// The caps: MaxCollections owned, MaxCollectionItems items (an add and a
// replace), MaxCollectionShares share users.
func TestCollectionLimits(t *testing.T) {
	e := newCollectionsEnv(t)
	c, ctx := e.c, t.Context()
	var first *Collection
	for i := range MaxCollections {
		col, err := c.CreateCollection(ctx, e.owner, fmt.Sprintf("C%d", i), "")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if first == nil {
			first = col
		}
	}
	if _, err := c.CreateCollection(ctx, e.owner, "one more", ""); !errors.Is(err, ErrCollectionsFull) {
		t.Fatalf("create past the cap = %v, want ErrCollectionsFull", err)
	}
	// Another user's cap is their own.
	if _, err := c.CreateCollection(ctx, e.stranger, "mine", ""); err != nil {
		t.Fatal(err)
	}

	for i := range MaxCollectionItems {
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,'t')`,
			first.ID, e.lib.ID, fmt.Sprintf("b%04d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.AddCollectionItem(ctx, first.ID, e.owner, e.ref("Kid/A"), nil); !errors.Is(err, ErrListFull) {
		t.Fatalf("add to a full collection = %v, want ErrListFull", err)
	}
	if err := c.SetCollectionItems(ctx, first.ID, e.owner, make([]Ref, MaxCollectionItems+1), e.all); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("replace past the cap = %v, want ErrTooManyItems", err)
	}
	if err := c.SetCollectionItems(ctx, first.ID, e.owner, make([]Ref, MaxCollectionItems), e.all); err != nil {
		t.Fatalf("replace at the cap: %v", err)
	}

	var ids []int64
	for i := range MaxCollectionShares + 1 {
		ids = append(ids, seedNamedUser(t, c, fmt.Sprintf("u%02d", i)))
	}
	if err := c.SetCollectionShares(ctx, first.ID, e.owner, ids); !errors.Is(err, ErrTooManyShares) {
		t.Fatalf("share with %d = %v, want ErrTooManyShares", len(ids), err)
	}
	// Duplicates collapse before counting.
	if err := c.SetCollectionShares(ctx, first.ID, e.owner, append(ids[:MaxCollectionShares], ids[0])); err != nil {
		t.Fatalf("share with %d: %v", MaxCollectionShares, err)
	}
	col, _ := c.Collection(ctx, first.ID, e.owner, e.all)
	if len(*col.SharedWith) != MaxCollectionShares {
		t.Fatalf("shared with %d, want %d", len(*col.SharedWith), MaxCollectionShares)
	}
}

// Shares name only existing, enabled, non-demo users other than the owner; one
// bad id rejects the whole request. Share targets list exactly those users.
func TestCollectionSharesAndTargets(t *testing.T) {
	e := newCollectionsEnv(t)
	c, ctx := e.c, t.Context()
	demo := seedNamedUser(t, c, "demo_1")
	disabled := seedNamedUser(t, c, "dora")
	if _, err := c.db.ExecContext(ctx, `UPDATE users SET is_demo = 1 WHERE id = ?`, demo); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, disabled); err != nil {
		t.Fatal(err)
	}
	col, _ := c.CreateCollection(ctx, e.owner, "Mine", "")
	if err := c.SetCollectionShares(ctx, col.ID, e.owner, []int64{e.viewer}); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]int64{"unknown": 9999, "demo": demo, "disabled": disabled, "self": e.owner} {
		if err := c.SetCollectionShares(ctx, col.ID, e.owner, []int64{e.stranger, id}); !errors.Is(err, ErrUnknownUser) {
			t.Errorf("share with %s = %v, want ErrUnknownUser", name, err)
		}
	}
	got, _ := c.Collection(ctx, col.ID, e.owner, e.all)
	if len(*got.SharedWith) != 1 || (*got.SharedWith)[0].ID != e.viewer {
		t.Fatalf("a rejected share changed the list: %+v", *got.SharedWith)
	}
	if err := c.SetCollectionShares(ctx, col.ID, e.owner, []int64{e.stranger}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Collection(ctx, col.ID, e.owner, e.all)
	if len(*got.SharedWith) != 1 || (*got.SharedWith)[0].ID != e.stranger {
		t.Fatalf("replaced shares = %+v", *got.SharedWith)
	}
	if _, err := c.Collection(ctx, col.ID, e.viewer, e.all); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unshared viewer = %v, want ErrNotFound", err)
	}

	targets, err := c.ShareTargets(ctx, e.owner)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range targets {
		names = append(names, u.Username)
	}
	if got := strings.Join(names, ","); got != "sam,vic" {
		t.Fatalf("share targets = %s, want sam,vic", got)
	}
}
