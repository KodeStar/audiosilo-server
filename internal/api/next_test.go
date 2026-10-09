package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// sagaMetaserve serves a four-book community series "Saga" (works one..four
// at positions 1..4). B0ONE..B0FOUR look up to their work, B0DOWN is an
// upstream outage and anything else is unmatched.
func sagaMetaserve(t *testing.T) *httptest.Server {
	t.Helper()
	works := map[string]string{"B0ONE": "one", "B0TWO": "two", "B0THREE": "three", "B0FOUR": "four"}
	positions := map[string]string{"one": "1", "two": "2", "three": "3", "four": "4"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		asin := r.URL.Query().Get("asin")
		id, ok := works[asin]
		switch {
		case asin == "B0DOWN":
			w.WriteHeader(http.StatusInternalServerError)
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"work":{"id":"` + id + `","title":"` + id + `","authors":[]},"recording_id":""}`))
		}
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		pos, ok := positions[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","title":"` + id + `","authors":[],"language":"en","series":[{"id":"saga","name":"Saga","position":"` + pos + `"}],"recordings":[]}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"saga","name":"Saga","authors":[],"works":[` +
			`{"position":"1","work":{"id":"one","title":"One","authors":[]}},` +
			`{"position":"2","work":{"id":"two","title":"Two","authors":[]}},` +
			`{"position":"3","work":{"id":"three","title":"Three","authors":[]}},` +
			`{"position":"4","work":{"id":"four","title":"Four","authors":[]}}]}`))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	return mock
}

// sagaEnv is a library holding Saga books, a member granted only "Saga/1" and
// "Saga/2", and an admin. Saga/3 is filed under a differently spelled series;
// Private/4 lies outside the member's grant.
type sagaEnv struct {
	*testEnv
	lib                 *catalog.Library
	adminTok, memberTok string
}

func (e *sagaEnv) url(endpoint, path string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(e.lib.ID, 10) + "/" + endpoint + "?path=" + escape(path)
}

func newSagaEnv(t *testing.T, metadataOn bool, books ...*catalog.Book) *sagaEnv {
	t.Helper()
	return newLibraryEnv(t, sagaMetaserve(t), metadataOn, books...)
}

// newLibraryEnv is newSagaEnv's library, member and admin over the metadata
// site mock.
func newLibraryEnv(t *testing.T, mock *httptest.Server, metadataOn bool, books ...*catalog.Book) *sagaEnv {
	t.Helper()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = metadataOn
		c.Metadata.BaseURL = mock.URL
	})
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range books {
		b.LibraryID, b.Title, b.Format, b.AddedAt = lib.ID, b.RelPath, "m4b", "2024-01-01T00:00:00Z"
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	adminTok, memberTok, memberID := adminAndMember(t, e)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Saga start"})
	if err := e.cat.AddSharePaths(ctx, share.ID, []catalog.PathRule{{LibraryID: lib.ID, Path: "Saga/1"}, {LibraryID: lib.ID, Path: "Saga/2"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	return &sagaEnv{testEnv: e, lib: lib, adminTok: adminTok, memberTok: memberTok}
}

// sagaBooks is the default Saga library.
func sagaBooks() []*catalog.Book {
	return []*catalog.Book{
		{RelPath: "Saga/1", Series: "Saga", SeriesIndex: 1, ASIN: "B0ONE"},
		{RelPath: "Saga/2", Series: "Saga", SeriesIndex: 2, ASIN: "B0TWO"},
		{RelPath: "Saga/3", Series: " Sága! ", SeriesIndex: 3},
		{RelPath: "Private/4", Series: "Saga", SeriesIndex: 4, ASIN: "B0FOUR"},
	}
}

// railLocals is the main rail's local path per work id ("" = not owned).
func railLocals(t *testing.T, e *sagaEnv, path, token string) map[string]string {
	t.Helper()
	resp, body := e.do(t, "GET", e.url("meta", path), token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta %s = %d %s", path, resp.StatusCode, body)
	}
	var env struct {
		Series []struct {
			Works []struct {
				ID    string `json:"id"`
				Local *struct {
					LibraryID int64  `json:"library_id"`
					Path      string `json:"path"`
				} `json:"local"`
			} `json:"works"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || len(env.Series) == 0 {
		t.Fatalf("decode %v: %s", err, body)
	}
	out := map[string]string{}
	for _, w := range env.Series[0].Works {
		out[w.ID] = ""
		if w.Local != nil {
			if w.Local.LibraryID != e.lib.ID {
				t.Fatalf("local library = %d", w.Local.LibraryID)
			}
			out[w.ID] = w.Local.Path
		}
	}
	return out
}

// TestMetaLocalPerCaller is the allowed+denied pair for `local`, and the cache
// rule: two callers with different grants get different locals from the SAME
// cached envelope, and the cached envelope itself is never annotated.
func TestMetaLocalPerCaller(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true, sagaBooks()...)

	// Allowed: the admin owns every entry; Saga/3 is found under its folded
	// series name, by its index.
	want := map[string]string{"one": "Saga/1", "two": "Saga/2", "three": "Saga/3", "four": "Private/4"}
	if got := railLocals(t, e, "Saga/2", e.adminTok); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin locals = %v, want %v", got, want)
	}
	// Denied: the member's request is a cache hit on the same envelope, but
	// the books outside their grant are never placed.
	want = map[string]string{"one": "Saga/1", "two": "Saga/2", "three": "", "four": ""}
	if got := railLocals(t, e, "Saga/2", e.memberTok); !reflect.DeepEqual(got, want) {
		t.Fatalf("member locals = %v, want %v", got, want)
	}
	// The admin again: the member's request left nothing behind.
	if got := railLocals(t, e, "Saga/2", e.adminTok); got["four"] != "Private/4" {
		t.Fatalf("admin locals after the member = %v", got)
	}
	// The cached envelope carries no local at all.
	env, err := e.api.meta.Enrich(context.Background(), "B0TWO", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rail := range env.Series {
		for _, w := range rail.Works {
			if w.Local != nil {
				t.Fatalf("cached envelope annotated: %+v", w)
			}
		}
	}
}

// TestMetaLocalPlacementFailure: `local` is an extra. When the caller's books
// can't be placed (here a share with more path rules than one SQLite expression
// holds), the envelope still goes out, without `local`, rather than failing a
// lookup that succeeded - a client that never reads `local` included.
func TestMetaLocalPlacementFailure(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true, sagaBooks()...)
	ctx := context.Background()
	u, err := e.auth.CreateUser(ctx, "wide", "wide-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Wide"})
	if err != nil {
		t.Fatal(err)
	}
	rules := []catalog.PathRule{{LibraryID: e.lib.ID, Path: "Saga/2"}}
	for i := range 1100 {
		rules = append(rules, catalog.PathRule{LibraryID: e.lib.ID, Path: "Pad/" + strconv.Itoa(i)})
	}
	if err := e.cat.AddSharePaths(ctx, share.ID, rules); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, u.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	scopes, err := e.cat.UserScopes(ctx, u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.SeriesBooks(ctx, scopes, []string{"Saga"}); err == nil {
		t.Skip("placing books no longer fails for a share this wide; this test needs another trigger")
	}
	tok, err := e.auth.IssueToken(ctx, u.ID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := railLocals(t, e, "Saga/2", tok) // fails the test unless 200 with rails
	if want := map[string]string{"one": "", "two": "", "three": "", "four": ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %v, want the rails without any", got)
	}
}

// TestMetaLocalCachedWorkID: a book whose enrichment the cache already holds is
// placed by its work id, beating a book numbered like the entry.
func TestMetaLocalCachedWorkID(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true, append(sagaBooks(),
		&catalog.Book{RelPath: "Extras/Three", Series: "Saga", ASIN: "B0THREE"})...) // unnumbered locally

	if got := railLocals(t, e, "Saga/2", e.adminTok); got["three"] != "Saga/3" {
		t.Fatalf("before its enrichment: three = %q, want Saga/3 by index", got["three"])
	}
	// Opening Extras/Three's panel caches its work id.
	railLocals(t, e, "Extras/Three", e.adminTok)
	if got := railLocals(t, e, "Saga/2", e.adminTok); got["three"] != "Extras/Three" {
		t.Fatalf("after its enrichment: three = %q, want Extras/Three by work id", got["three"])
	}
}

// nextBody is GET /next's answer as a client reads it.
type nextBody struct {
	Source string `json:"source"`
	Next   *struct {
		LibraryID int64  `json:"library_id"`
		Path      string `json:"path"`
	} `json:"next"`
	Book *struct {
		RelPath string `json:"rel_path"`
	} `json:"book"`
	Work *struct {
		ID    string          `json:"id"`
		Local json.RawMessage `json:"local"`
	} `json:"work"`
	raw string
}

func getNext(t *testing.T, e *testEnv, url, token string) nextBody {
	t.Helper()
	resp, body := e.do(t, "GET", url, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, body)
	}
	var n nextBody
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	n.raw = body
	return n
}

// nextPath is the next book's path ("" = none).
func (n nextBody) nextPath() string {
	if n.Next == nil {
		return ""
	}
	return n.Next.Path
}

func TestNextCommunity(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true, sagaBooks()...)

	// Owned: next + book + work (with its local). The book is the list shape.
	n := getNext(t, e.testEnv, e.url("next", "Saga/2"), e.adminTok)
	if n.Source != nextCommunity || n.nextPath() != "Saga/3" || n.Next.LibraryID != e.lib.ID ||
		n.Book == nil || n.Book.RelPath != "Saga/3" || n.Work == nil || n.Work.ID != "three" || n.Work.Local == nil {
		t.Fatalf("owned = %s", n.raw)
	}
	for _, field := range []string{`"files"`, `"chapters"`, `"description"`} {
		if strings.Contains(n.raw, field) {
			t.Fatalf("book is not the list shape (%s): %s", field, n.raw)
		}
	}

	// Not placed (denied: Saga/3 is outside the member's grant): placing proves
	// nothing either way, so the local steps answer - here the end of the
	// member's numbered Saga (Saga/1 is the only other) - with the community's
	// next work beside it, without `local`. Nothing outside the grant leaks.
	n = getNext(t, e.testEnv, e.url("next", "Saga/2"), e.memberTok)
	if n.Source != nextSeries || n.Next != nil || n.Book != nil || n.Work == nil || n.Work.ID != "three" || n.Work.Local != nil {
		t.Fatalf("not placed = %s", n.raw)
	}
	if strings.Contains(n.raw, "Saga/3") || strings.Contains(n.raw, "Private/4") {
		t.Fatalf("a book outside the grant leaked: %s", n.raw)
	}
}

// TestNextCommunityUnplaced: a next rail entry the caller's books can't be
// placed on (untagged, or tagged under a series named unlike the rail) does not
// stop the lookup: the series or folder step answers, with the community's next
// work attached without `local`.
func TestNextCommunityUnplaced(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true,
		&catalog.Book{RelPath: "Loose/1", ASIN: "B0ONE", IsFolder: true}, // untagged
		&catalog.Book{RelPath: "Loose/2", IsFolder: true},
		&catalog.Book{RelPath: "Chron/1", Series: "Saga Chronicles", SeriesIndex: 1, ASIN: "B0ONE"},
		&catalog.Book{RelPath: "Chron/2", Series: "Saga Chronicles", SeriesIndex: 2},
	)
	mkdirs(t, e.lib.Root, "Loose/1", "Loose/2", "Chron/1", "Chron/2")

	for from, want := range map[string]struct{ source, next string }{
		"Loose/1": {nextFolder, "Loose/2"},
		"Chron/1": {nextSeries, "Chron/2"},
	} {
		n := getNext(t, e.testEnv, e.url("next", from), e.adminTok)
		if n.Source != want.source || n.nextPath() != want.next || n.Book == nil || n.Book.RelPath != want.next ||
			n.Work == nil || n.Work.ID != "two" || n.Work.Local != nil {
			t.Fatalf("%s = %s, want %s %s with work two unplaced", from, n.raw, want.source, want.next)
		}
	}
}

// TestNextCommunityLast: the current work last on the rail does not end the
// series either - the rail can lag the library - so the local steps answer,
// with no community work.
func TestNextCommunityLast(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true, append(sagaBooks(),
		&catalog.Book{RelPath: "Saga/5", Series: "Saga", SeriesIndex: 5},
		&catalog.Book{RelPath: "Late/4", ASIN: "B0FOUR", IsFolder: true}, // untagged
		&catalog.Book{RelPath: "Late/5", IsFolder: true},
	)...)
	mkdirs(t, e.lib.Root, "Late/4", "Late/5")

	for from, want := range map[string]struct{ source, next string }{
		"Private/4": {nextSeries, "Saga/5"},
		"Late/4":    {nextFolder, "Late/5"},
	} {
		n := getNext(t, e.testEnv, e.url("next", from), e.adminTok)
		if n.Source != want.source || n.nextPath() != want.next || n.Work != nil {
			t.Fatalf("%s = %s, want %s %s without work", from, n.raw, want.source, want.next)
		}
	}
	// Nothing locally either: none, still without work.
	e = newSagaEnv(t, true, &catalog.Book{RelPath: "Only/4", ASIN: "B0FOUR", IsFolder: true})
	mkdirs(t, e.lib.Root, "Only/4")
	if n := getNext(t, e.testEnv, e.url("next", "Only/4"), e.adminTok); n.Source != nextNone || n.Next != nil || n.Work != nil {
		t.Fatalf("last with nothing local = %s", n.raw)
	}
}

func TestNextCommunityFallsThrough(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, true,
		&catalog.Book{RelPath: "Down/1", Series: "Down", SeriesIndex: 1, ASIN: "B0DOWN"},
		&catalog.Book{RelPath: "Down/2", Series: "Down", SeriesIndex: 2},
		&catalog.Book{RelPath: "None/1", Series: "None", SeriesIndex: 1, ASIN: "B0NOMATCH"},
		&catalog.Book{RelPath: "None/2", Series: "None", SeriesIndex: 2},
	)
	// An upstream outage and an unmatched book both fall through to the local
	// series.
	for from, want := range map[string]string{"Down/1": "Down/2", "None/1": "None/2"} {
		if n := getNext(t, e.testEnv, e.url("next", from), e.adminTok); n.Source != nextSeries || n.nextPath() != want {
			t.Fatalf("%s = %s, want series %s", from, n.raw, want)
		}
	}
}

func TestNextSeries(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, false,
		&catalog.Book{RelPath: "Saga/1", Series: "Saga", SeriesIndex: 1, ASIN: "B0ONE"},
		&catalog.Book{RelPath: "Other/1.5", Series: "Saga", SeriesIndex: 1.5},
		&catalog.Book{RelPath: "Saga/2", Series: "Saga", SeriesIndex: 2},
	)
	// Metadata off: the local series decides. Allowed: the admin's next is the
	// smallest later index.
	n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.adminTok)
	if n.Source != nextSeries || n.nextPath() != "Other/1.5" || n.Book == nil || n.Book.RelPath != "Other/1.5" || n.Work != nil {
		t.Fatalf("admin = %s", n.raw)
	}
	// Denied: Other/1.5 is outside the member's grant, so it is skipped.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok); n.nextPath() != "Saga/2" || strings.Contains(n.raw, "Other/") {
		t.Fatalf("member = %s", n.raw)
	}
	// The end of the series.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/2"), e.adminTok); n.Source != nextSeries || n.Next != nil {
		t.Fatalf("end = %s", n.raw)
	}
}

// mkdirs creates folders (each holding an audio file) under root.
func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "part.mp3"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNextFolder(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, false,
		&catalog.Book{RelPath: "Saga/1", IsFolder: true},
		&catalog.Book{RelPath: "Saga/10", IsFolder: true},
		&catalog.Book{RelPath: "Loose/Part A", IsFolder: true},
		&catalog.Book{RelPath: "Alone/Only", IsFolder: true},
	)
	mkdirs(t, e.lib.Root, "Saga/1", "Saga/2", "Saga/10", "Saga/Extras", "Loose/Part A", "Loose/Part B", "Alone/Only")

	// The next indexed book, in natural order, over the unindexed Saga/2 (the
	// folder holds other indexed books, so a bare folder is not a book).
	n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.adminTok)
	if n.Source != nextFolder || n.nextPath() != "Saga/10" || n.Book == nil || n.Book.RelPath != "Saga/10" {
		t.Fatalf("indexed = %s", n.raw)
	}
	// Nothing else in the folder indexed: the current book is, so a bare folder
	// after it is not offered (it may be a series or author folder, which would
	// strand the player), as the player's own folder walk decides.
	n = getNext(t, e.testEnv, e.url("next", "Loose/Part A"), e.adminTok)
	if n.Source != nextNone || n.Next != nil || n.Book != nil {
		t.Fatalf("unindexed = %s", n.raw)
	}
	// Denied: the member is granted Saga/1 and Saga/2 only; Saga/10 is not
	// theirs, and Saga/2 is not indexed yet, so nothing follows for them.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok); n.Source != nextNone || strings.Contains(n.raw, "Saga/10") {
		t.Fatalf("member = %s", n.raw)
	}
	// Allowed: once Saga/2 is indexed it is the member's next book.
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: e.lib.ID, RelPath: "Saga/2", IsFolder: true,
		Title: "Saga/2", Format: "m4b", AddedAt: "2024-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok); n.nextPath() != "Saga/2" || n.Book == nil || strings.Contains(n.raw, "Saga/10") {
		t.Fatalf("member after Saga/2 is indexed = %s", n.raw)
	}
	// Alone in its folder: none.
	if n := getNext(t, e.testEnv, e.url("next", "Alone/Only"), e.adminTok); n.Source != nextNone || n.Next != nil || n.Book != nil {
		t.Fatalf("alone = %s", n.raw)
	}
}

func TestNextErrors(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, false, sagaBooks()...)
	for name, tc := range map[string]struct {
		url, token string
		want       int
	}{
		"bad library id":    {"/api/v1/libraries/abc/next?path=x", e.adminTok, http.StatusBadRequest},
		"missing path":      {"/api/v1/libraries/" + strconv.FormatInt(e.lib.ID, 10) + "/next", e.adminTok, http.StatusBadRequest},
		"outside the grant": {e.url("next", "Private/4"), e.memberTok, http.StatusForbidden},
		"unknown library":   {"/api/v1/libraries/999/next?path=x", e.adminTok, http.StatusNotFound},
		"no book there":     {e.url("next", "Nowhere/Book"), e.adminTok, http.StatusNotFound},
		"no library access": {"/api/v1/libraries/999/next?path=x", e.memberTok, http.StatusForbidden},
		"not signed in":     {e.url("next", "Saga/1"), "", http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			if resp, body := e.do(t, "GET", tc.url, tc.token, ""); resp.StatusCode != tc.want {
				t.Fatalf("= %d %s, want %d", resp.StatusCode, body, tc.want)
			}
		})
	}
}

func TestNextBookCapability(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"next_book":true`) {
		t.Fatalf("/server missing next_book: %s", si)
	}
}

// setMoreSeries gives the book at path its other series (a more_series list).
func setMoreSeries(t *testing.T, e *sagaEnv, path, list string) {
	t.Helper()
	if err := e.cat.EditBook(context.Background(), e.lib.ID, path, catalog.BookEdit{Set: map[string]string{catalog.FieldMoreSeries: list}}); err != nil {
		t.Fatal(err)
	}
}

// TestNextSeriesMemberships: the local step follows every series a book is in
// (catalog.NextInSeries, whose policy catalog's tests cover): when the main
// series has ended a series the book lists answers, and the grant narrows it
// (allowed and denied).
func TestNextSeriesMemberships(t *testing.T) {
	t.Parallel()
	e := newSagaEnv(t, false,
		&catalog.Book{RelPath: "Saga/1", Series: "Discworld", SeriesIndex: 8},         // City Watch 1 (list)
		&catalog.Book{RelPath: "Saga/2/Feet of Clay", Series: "Ankh", SeriesIndex: 1}, // City Watch 3 (list)
		&catalog.Book{RelPath: "Private/Men at Arms", Series: "City Watch", SeriesIndex: 2},
	)
	setMoreSeries(t, e, "Saga/1", `[{"name":"City Watch","position":1}]`)
	setMoreSeries(t, e, "Saga/2/Feet of Clay", `[{"name":"City Watch","position":3}]`)

	// Allowed: Discworld holds nothing after #8, so City Watch answers: Men at
	// Arms, #2 by its main series.
	n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.adminTok)
	if n.Source != nextSeries || n.nextPath() != "Private/Men at Arms" || n.Book == nil || n.Book.RelPath != "Private/Men at Arms" {
		t.Fatalf("admin = %s", n.raw)
	}
	// Denied: Men at Arms is outside the member's grant; Feet of Clay, City Watch
	// #3 through its list, is theirs.
	n = getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok)
	if n.Source != nextSeries || n.nextPath() != "Saga/2/Feet of Clay" || strings.Contains(n.raw, "Men at Arms") {
		t.Fatalf("member = %s", n.raw)
	}
}

// twoRailMetaserve serves a work "cur" (B0CUR) in two community series, Alpha
// and Beta (listed in that order), at #1 in each; a2 and b2 are #2 of each.
func twoRailMetaserve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asin") != "B0CUR" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"cur","title":"Cur","authors":[]},"recording_id":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "cur" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"cur","title":"Cur","authors":[],"language":"en","series":[` +
			`{"id":"alpha","name":"Alpha","position":"1"},{"id":"beta","name":"Beta","position":"1"}],"recordings":[]}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		name := map[string]string{"alpha": "Alpha", "beta": "Beta"}[id]
		if name == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"` + name + `","authors":[],"works":[` +
			`{"position":"1","work":{"id":"cur","title":"Cur","authors":[]}},` +
			`{"position":"2","work":{"id":"` + id[:1] + `2","title":"Two","authors":[]}}]}`))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	return mock
}

// narniaMetaserve serves The Silver Chair ("sc", B0SC) on two community rails:
// Narnia (chronological, where it is #6 and last) and Narnia (Publication),
// where it is #4 and The Horse and His Boy (#5), The Magician's Nephew (#6) and
// The Last Battle (#7) follow; the first two are earlier chronologically.
func narniaMetaserve(t *testing.T) *httptest.Server {
	t.Helper()
	entries := func(works ...string) string {
		var parts []string
		for i, w := range works {
			if w != "" {
				parts = append(parts, `{"position":"`+strconv.Itoa(i+1)+`","work":{"id":"`+w+`","title":"`+w+`","authors":[]}}`)
			}
		}
		return strings.Join(parts, ",")
	}
	rails := map[string]string{
		"chrono": `{"id":"chrono","name":"Narnia","authors":[],"works":[` + entries("mn", "lww", "hhb", "pc", "vdt", "sc") + `]}`,
		"pub":    `{"id":"pub","name":"Narnia (Publication)","authors":[],"works":[` + entries("lww", "pc", "vdt", "sc", "hhb", "mn", "lb") + `]}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asin") != "B0SC" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"sc","title":"The Silver Chair","authors":[]},"recording_id":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "sc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"sc","title":"The Silver Chair","authors":[],"language":"en","series":[` +
			`{"id":"chrono","name":"Narnia","position":"6"},{"id":"pub","name":"Narnia (Publication)","position":"4"}],"recordings":[]}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, ok := rails[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	return mock
}

// TestNextCommunityPassesOverStepsBack: the chronological rail has ended, so the
// publication rail decides, with its first later entry that doesn't loop back
// chronologically: The Last Battle, passing over The Horse and His Boy, which the
// caller owns and which is placed. The answer is the entry NextRail chose, read
// off the placed rails, not that rail's plain next entry (the owned The Horse and
// His Boy); without The Last Battle owned it rides along unplaced beside the
// local step, which passes over the same steps back and ends numbered.
func TestNextCommunityPassesOverStepsBack(t *testing.T) {
	t.Parallel()
	for name, ownLast := range map[string]bool{"owned: community": true, "unowned: the local series ends": false} {
		t.Run(name, func(t *testing.T) {
			books := []*catalog.Book{
				{RelPath: "Narnia/6 The Silver Chair", Series: "Narnia", SeriesIndex: 6, ASIN: "B0SC", IsFolder: true},
				{RelPath: "Narnia/3 The Horse and His Boy", Series: "Narnia", SeriesIndex: 3},
			}
			if ownLast {
				books = append(books, &catalog.Book{RelPath: "Narnia/7 The Last Battle", Series: "Narnia (Publication)", SeriesIndex: 7})
			}
			e := newLibraryEnv(t, narniaMetaserve(t), true, books...)
			mkdirs(t, e.lib.Root, "Narnia/6 The Silver Chair")
			setMoreSeries(t, e, "Narnia/6 The Silver Chair", `[{"name":"Narnia (Publication)","position":4}]`)
			setMoreSeries(t, e, "Narnia/3 The Horse and His Boy", `[{"name":"Narnia (Publication)","position":5}]`)
			n := getNext(t, e.testEnv, e.url("next", "Narnia/6 The Silver Chair"), e.adminTok)
			if n.Work == nil || n.Work.ID != "lb" {
				t.Fatalf("work = %s, want lb", n.raw)
			}
			if ownLast {
				if n.Source != nextCommunity || n.nextPath() != "Narnia/7 The Last Battle" || n.Work.Local == nil ||
					n.Book == nil || n.Book.RelPath != "Narnia/7 The Last Battle" {
					t.Fatalf("= %s, want community The Last Battle", n.raw)
				}
				return
			}
			if n.Source != nextSeries || n.Next != nil || n.Work.Local != nil {
				t.Fatalf("= %s, want the series ended with lb unplaced", n.raw)
			}
		})
	}
}

// TestNextCommunityRails: the rails of a book in several community series are
// taken in the book's own order (meta.NextRail, whose policy meta's tests cover;
// here a rail named like a listed series comes before the envelope's first), and
// the first with a next entry decides: its entry answers when it is the caller's
// book, and when it isn't, a later rail's placed entry doesn't jump the series -
// the local steps answer (the main series continuing locally, Discworld #8 to
// #10 with #9 unowned) with that entry beside them, without `local`.
func TestNextCommunityRails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		series, more string   // the current book's main series (at #1) and list
		owned        []string // the #2 books held: "Alpha", "Beta"
		third        bool     // a local Beta #3, on no rail
		source, next string
		work         string
	}{
		"a listed series' rail first":   {"Gamma", `[{"name":"Beta","position":1}]`, []string{"Alpha", "Beta"}, false, nextCommunity, "B/2", "b2"},
		"the first placed rail decides": {"Alpha", "", []string{"Alpha", "Beta"}, false, nextCommunity, "A/2", "a2"},
		// Beta decides with b2, unowned: Alpha's placed a2 doesn't answer; the
		// main series continues locally at #3.
		"unplaced: the local series continues": {"Beta", "", []string{"Alpha"}, true, nextSeries, "B/3", "b2"},
		"none placed: the first's work":        {"Beta", `[{"name":"Alpha","position":1}]`, nil, false, nextNone, "", "b2"},
	} {
		t.Run(name, func(t *testing.T) {
			books := []*catalog.Book{{RelPath: "X/cur", Series: tc.series, SeriesIndex: 1, ASIN: "B0CUR", IsFolder: true}}
			owned := map[string]string{} // series -> the path of its #2
			for _, s := range tc.owned {
				owned[s] = s[:1] + "/2"
				books = append(books, &catalog.Book{RelPath: owned[s], Series: s + " Shelf", SeriesIndex: 2})
			}
			if tc.third {
				books = append(books, &catalog.Book{RelPath: "B/3", Series: tc.series, SeriesIndex: 3})
			}
			e := newLibraryEnv(t, twoRailMetaserve(t), true, books...)
			mkdirs(t, e.lib.Root, "X/cur")
			if tc.more != "" {
				setMoreSeries(t, e, "X/cur", tc.more)
			}
			// The #2 books are in the rails' series through their lists, so the
			// rails place them there.
			for s, path := range owned {
				setMoreSeries(t, e, path, `[{"name":"`+s+`","position":2}]`)
			}
			n := getNext(t, e.testEnv, e.url("next", "X/cur"), e.adminTok)
			if n.Source != tc.source || n.nextPath() != tc.next || n.Work == nil || n.Work.ID != tc.work {
				t.Fatalf("= %s, want %s %q with work %s", n.raw, tc.source, tc.next, tc.work)
			}
			if (tc.source == nextCommunity) != (n.Work.Local != nil) || (tc.next != "" && (n.Book == nil || n.Book.RelPath != tc.next)) {
				t.Fatalf("local/book do not match next: %s", n.raw)
			}
		})
	}
}
