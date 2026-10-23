package catalog

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// gateEnv is a user with whole-library grants on a books library (A, B, C) and a
// podcast library holding one public show (its episodes eps), and the two
// callers it can be: a player that sees podcasts and an old one that doesn't.
type gateEnv struct {
	c                *Catalog
	ctx              context.Context
	user             int64
	books, pods      *Library
	eps              []string
	newer, old       Caller
	newScopes        []Scope
	oldScopes, every []Scope
}

func newGateEnv(t *testing.T) gateEnv {
	t.Helper()
	c, ctx := newTestCatalog(t)
	e := gateEnv{c: c, ctx: ctx, user: seedUser(t, c, ctx)}
	root := t.TempDir()
	var err error
	if e.books, err = c.CreateLibrary(ctx, Library{Name: "Books", Root: filepath.Join(root, "books")}); err != nil {
		t.Fatal(err)
	}
	if e.pods, err = c.CreateLibrary(ctx, Library{Name: "Pods", Root: filepath.Join(root, "pods"), Kind: KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A", "B", "C"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: e.books.ID, RelPath: p, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, e.pods.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, ep := range f.Episodes {
		e.eps = append(e.eps, ep.Path)
	}
	for _, id := range []int64{e.books.ID, e.pods.ID} {
		if err := c.GrantWholeLibrary(ctx, e.user, id); err != nil {
			t.Fatal(err)
		}
	}
	e.newer = Caller{ID: e.user, Podcasts: true}
	e.old = Caller{ID: e.user}
	e.newScopes = e.scopes(t, e.newer)
	e.oldScopes = e.scopes(t, e.old)
	e.every = []Scope{{LibraryID: e.books.ID, AllowAll: true}, {LibraryID: e.pods.ID, AllowAll: true}}
	return e
}

func (e gateEnv) scopes(t *testing.T, who Caller) []Scope {
	t.Helper()
	s, err := e.c.CallerScopes(e.ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e gateEnv) gated(t *testing.T, who Caller) []Scope {
	t.Helper()
	scopes, keep, err := e.c.GatedScopes(e.ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	if want := e.scopes(t, who); !slices.EqualFunc(scopes, want, func(a, b Scope) bool {
		return a.LibraryID == b.LibraryID && a.AllowAll == b.AllowAll && slices.Equal(a.Paths, b.Paths)
	}) {
		t.Fatalf("replace scopes %+v, want CallerScopes %+v", scopes, want)
	}
	return keep
}

func (e gateEnv) book(p string) Ref { return Ref{LibraryID: e.books.ID, Path: p} }
func (e gateEnv) ep(i int) Ref      { return Ref{LibraryID: e.pods.ID, Path: e.eps[i]} }

// queue stores refs as the user's queue, added in order by a podcast-seeing player.
func (e gateEnv) queue(t *testing.T, refs ...Ref) {
	t.Helper()
	for _, r := range refs {
		if err := e.c.AddToQueue(e.ctx, e.user, r, nil, e.newScopes); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatedScopes(t *testing.T) {
	e := newGateEnv(t)
	if s := e.gated(t, e.newer); s != nil {
		t.Fatalf("a podcast-seeing caller has gated scopes %+v", s)
	}
	s := e.gated(t, e.old)
	if len(s) != 1 || s[0].LibraryID != e.pods.ID || !s[0].AllowAll {
		t.Fatalf("old caller's gated scopes %+v", s)
	}
}

// An old player's replace keeps the podcast entries it can't see, each after the
// listed book it followed; a reorder that would shuffle them appends them.
func TestSetQueueOldClientKeepsPodcastEntries(t *testing.T) {
	e := newGateEnv(t)
	e.queue(t, e.book("A"), e.ep(0), e.book("B"), e.ep(1), e.book("C"))
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.book("C"), e.book("A")}, e.oldScopes, e.gated(t, e.old)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "old player's view", queuePaths(t, e.c, e.user, e.oldScopes), "C", "A")
	wantPaths(t, "stored", queuePaths(t, e.c, e.user, e.every), "C", "A", e.eps[0], e.eps[1])

	// Episodes after A and after C: listing C before A would swap them, so
	// they go to the end in their stored order.
	if err := e.c.SetQueue(e.ctx, e.user, nil, e.newScopes, nil); err != nil {
		t.Fatal(err)
	}
	e.queue(t, e.book("A"), e.ep(0), e.book("C"), e.ep(1))
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.book("C"), e.book("B"), e.book("A")}, e.oldScopes, e.gated(t, e.old)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "reordered", queuePaths(t, e.c, e.user, e.every), "C", "B", "A", e.eps[0], e.eps[1])

	// An episode queued before any book stays first.
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.ep(2), e.book("A")}, e.newScopes, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.book("B"), e.book("A")}, e.oldScopes, e.gated(t, e.old)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "leading episode", queuePaths(t, e.c, e.user, e.every), e.eps[2], "B", "A")
}

// A podcast-seeing player's replace still deletes what it leaves out.
func TestSetQueueNewClientDeletesOmitted(t *testing.T) {
	e := newGateEnv(t)
	e.queue(t, e.book("A"), e.ep(0), e.ep(1))
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.ep(1)}, e.newScopes, e.gated(t, e.newer)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "stored", queuePaths(t, e.c, e.user, e.every), e.eps[1])
}

// An episode hidden because the share was revoked is deleted as before, even by
// an old player's replace.
func TestSetQueueRevokedPodcastShareStillDeleted(t *testing.T) {
	e := newGateEnv(t)
	e.queue(t, e.book("A"), e.ep(0))
	share, err := e.c.shareByName(e.ctx, "Library: Pods")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.RevokeShare(e.ctx, e.user, share.ID); err != nil {
		t.Fatal(err)
	}
	keep := e.gated(t, e.old)
	if ScopesAllow(keep, e.ep(0)) {
		t.Fatalf("revoked share still gated-kept: %+v", keep)
	}
	if err := e.c.SetQueue(e.ctx, e.user, []Ref{e.book("A")}, e.scopes(t, e.old), keep); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "stored", queuePaths(t, e.c, e.user, e.every), "A")
}

func TestSetCollectionItemsOldClientKeepsPodcastEntries(t *testing.T) {
	e := newGateEnv(t)
	col, err := e.c.CreateCollection(e.ctx, e.user, "Mixed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.SetCollectionItems(e.ctx, col.ID, e.user, []Ref{e.book("A"), e.ep(0), e.book("B")}, e.newScopes, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.c.SetCollectionItems(e.ctx, col.ID, e.user, []Ref{e.book("B")}, e.oldScopes, e.gated(t, e.old)); err != nil {
		t.Fatal(err)
	}
	// A, which the episode followed, is gone: no listed row preceded it, so it leads.
	wantPaths(t, "old player kept", itemPaths(t, e.c, col.ID, e.user, e.every), e.eps[0], "B")
	if err := e.c.SetCollectionItems(e.ctx, col.ID, e.user, []Ref{e.book("B")}, e.newScopes, e.gated(t, e.newer)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "new player deleted", itemPaths(t, e.c, col.ID, e.user, e.every), "B")
}

// Kept rows count toward the cap as add counts hidden rows: the oldest go.
func TestKeptRowsCap(t *testing.T) {
	l := orderedList{max: 3}
	keep := []Scope{{LibraryID: 2, AllowAll: true}}
	row := func(lib int64, p, at string) listRow { return listRow{Ref: Ref{LibraryID: lib, Path: p}, AddedAt: at} }
	rows := []listRow{row(2, "new", "3"), row(1, "a", "1"), row(2, "old", "1"), row(2, "mid", "2"), row(1, "gone", "0")}
	listed := map[Ref]bool{{LibraryID: 1, Path: "a"}: true}
	paths := func(rs []listRow) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, r.Path)
		}
		return out
	}
	if got := paths(l.keptRows(rows, listed, keep, 1)); !slices.Equal(got, []string{"new", "mid"}) {
		t.Fatalf("kept %v", got)
	}
	if got := paths(l.keptRows(rows, listed, keep, 0)); !slices.Equal(got, []string{"new", "old", "mid"}) {
		t.Fatalf("kept under the cap %v", got)
	}
	if got := l.keptRows(rows, listed, nil, 1); len(got) != 0 {
		t.Fatalf("nothing to keep %v", got)
	}
}
