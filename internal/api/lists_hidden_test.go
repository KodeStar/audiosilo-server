package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Hidden rows: entries outside the caller's CURRENT access are kept but never
// shown or counted, so an add's position is an index in the list as the caller
// sees it, and the cap counts only what they see (an add the hidden rows alone
// would overflow evicts the oldest of them).

// hiddenList is the queue or one collection, as kid (the "Will Wight" folder
// only) writes and reads it.
type hiddenList struct {
	name  string
	url   string // POST and PUT
	max   int
	code  string // the full error's code
	paths func(t *testing.T) string
}

func (l *listsEnv) hiddenLists(t *testing.T) []hiddenList {
	col := l.createCollection(t, l.kidTok, "Kid's")
	return []hiddenList{
		{name: "queue", url: "/api/v1/me/queue", max: catalog.MaxQueue, code: "queue_full",
			paths: func(t *testing.T) string { t.Helper(); return itemPathsOf(l.queueOf(t, l.kidTok)) }},
		{name: "collection", url: colURL(col.ID, "/items"), max: catalog.MaxCollectionItems, code: "collection_full",
			paths: func(t *testing.T) string {
				t.Helper()
				d, _ := l.detail(t, l.kidTok, col.ID)
				if d.Collection.ItemCount != len(d.Items) {
					t.Fatalf("item_count %d, %d items", d.Collection.ItemCount, len(d.Items))
				}
				return itemPathsOf(d.Items)
			}},
	}
}

// sandersonShare makes a "Brandon Sanderson" (Mistborn) share and returns
// functions granting it to kid and revoking it: what kid adds there while it is
// granted turns hidden once it is revoked.
func (l *listsEnv) sandersonShare(t *testing.T) (grant, revoke func()) {
	t.Helper()
	ctx := context.Background()
	share, err := l.cat.CreateShare(ctx, catalog.Share{Name: "Sanderson",
		Paths: []catalog.PathRule{{LibraryID: l.libID, Path: "Brandon Sanderson"}}})
	if err != nil {
		t.Fatal(err)
	}
	grant = func() {
		if err := l.cat.GrantShare(ctx, l.kid, share.ID); err != nil {
			t.Fatal(err)
		}
	}
	revoke = func() {
		if err := l.cat.RevokeShare(ctx, l.kid, share.ID); err != nil {
			t.Fatal(err)
		}
	}
	return grant, revoke
}

// A position counts the rows the caller sees: with a hidden row in front, a
// move to index 1 lands below the visible row there (not a no-op), an insert at
// a visible index lands before the visible row at it, and the hidden row stays
// where it was.
func TestListPositionIsAVisibleIndex(t *testing.T) {
	l := newListsEnv(t)
	ghost := "Will Wight/Ghostwater"
	if _, err := l.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: l.libID, RelPath: ghost, IsFolder: true,
		Title: "Ghostwater", Author: "Will Wight"}); err != nil {
		t.Fatal(err)
	}
	grant, revoke := l.sandersonShare(t)
	for _, list := range l.hiddenLists(t) {
		t.Run(list.name, func(t *testing.T) {
			post := func(path string, pos int) {
				t.Helper()
				if resp, b := l.do(t, "POST", list.url, l.kidTok, l.addJSON(path, pos)); resp.StatusCode != http.StatusOK {
					t.Fatalf("POST %s at %d = %d %s", path, pos, resp.StatusCode, b)
				}
			}
			want := func(what, want string) {
				t.Helper()
				if got := list.paths(t); got != want {
					t.Fatalf("%s = %s, want %s", what, got, want)
				}
			}
			grant()
			post(mistbornBook, -1)
			post(cradleBook, -1)
			post(thread, -1)
			revoke() // stored [Mistborn (hidden), Cradle, Threadlight]
			want("visible", cradleBook+"|"+thread)

			post(cradleBook, 1)
			want("after moving Cradle to 1", thread+"|"+cradleBook)
			post(ghost, 1)
			want("after inserting at 1", thread+"|"+ghost+"|"+cradleBook)
			post(ghost, 0)
			want("after moving to 0", ghost+"|"+thread+"|"+cradleBook)
			post(ghost, 99)
			want("after moving past the end", thread+"|"+cradleBook+"|"+ghost)

			grant()
			want("re-granted", mistbornBook+"|"+thread+"|"+cradleBook+"|"+ghost)
			revoke()
		})
	}
}

// The cap counts visible rows: a list full only because of a hidden row takes
// a new book by evicting the hidden row (gone for good once access returns),
// and a list whose visible rows alone are at the cap is still 409.
func TestListCapCountsVisibleRows(t *testing.T) {
	l := newListsEnv(t)
	ctx := context.Background()
	pad := make([]string, catalog.MaxCollectionItems)
	for i := range pad {
		pad[i] = fmt.Sprintf("Will Wight/x%04d", i)
		if _, err := l.cat.UpsertBook(ctx, &catalog.Book{LibraryID: l.libID, RelPath: pad[i], IsFolder: true,
			Title: pad[i]}); err != nil {
			t.Fatal(err)
		}
	}
	grant, revoke := l.sandersonShare(t)
	for _, list := range l.hiddenLists(t) {
		t.Run(list.name, func(t *testing.T) {
			post := func(path string) (int, string) {
				t.Helper()
				resp, b := l.do(t, "POST", list.url, l.kidTok, l.addJSON(path, -1))
				return resp.StatusCode, b
			}
			// max-1 visible rows, then one that turns hidden: stored full.
			if resp, b := l.do(t, "PUT", list.url, l.kidTok, l.itemsJSON(pad[:list.max-1]...)); resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT = %d %s", resp.StatusCode, b)
			}
			grant()
			if code, b := post(mistbornBook); code != http.StatusOK {
				t.Fatalf("add Mistborn = %d %s", code, b)
			}
			revoke()
			if got := strings.Count(list.paths(t), "|") + 1; got != list.max-1 {
				t.Fatalf("visible = %d, want %d", got, list.max-1)
			}

			if code, b := post(cradleBook); code != http.StatusOK {
				t.Fatalf("add to a list full only of hidden rows = %d %s, want 200", code, b)
			}
			paths := list.paths(t)
			if n := strings.Count(paths, "|") + 1; n != list.max || !strings.HasSuffix(paths, "|"+cradleBook) {
				t.Fatalf("after the add: %d visible, ends %q", n, paths[strings.LastIndex(paths, "|"):])
			}
			if code, b := post(thread); code != http.StatusConflict || !strings.Contains(b, `"code":"`+list.code+`"`) {
				t.Fatalf("add to a visibly full list = %d %s, want 409 %s", code, b, list.code)
			}

			// The evicted row is gone: access back shows only what survived.
			grant()
			if got := list.paths(t); got != paths {
				t.Fatalf("re-granted list differs from what was shown (the hidden row survived?)")
			}
			revoke()
		})
	}
}
