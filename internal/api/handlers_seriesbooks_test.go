package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// seriesBooksBody is GET /libraries/{id}/series/books decoded loosely, so a
// test compares each entry with the matching /books page field for field.
type seriesBooksBody struct {
	Series []map[string]any `json:"series"`
}

// newSeriesBooksEnv is the browse env with Saga Two and the out-of-grant Other
// One also book 1 of "Spin-off" (through more_series), as TestSeriesMemberships.
func newSeriesBooksEnv(t *testing.T) *browseEnv {
	t.Helper()
	e := newBrowseEnv(t)
	for _, p := range []string{"In/A2", "Out/B1"} {
		if err := e.cat.EditBook(context.Background(), e.libID, p, catalog.BookEdit{Set: map[string]string{
			catalog.FieldMoreSeries: `[{"name":"Spin-off","position":1}]`}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// seriesBooksQuery builds ?name=...&name=...[&extra].
func seriesBooksQuery(names []string, extra string) string {
	q := url.Values{"name": names}.Encode()
	if extra != "" {
		q += "&" + extra
	}
	return "?" + q
}

// entryNames is the entries' names in response order.
func entryNames(b seriesBooksBody) []string {
	out := []string{}
	for _, s := range b.Series {
		name, _ := s["name"].(string)
		out = append(out, name)
	}
	return out
}

// entryPaths is one entry's books' rel_paths in order.
func entryPaths(t *testing.T, entry map[string]any) []string {
	t.Helper()
	books, ok := entry["books"].([]any)
	if !ok {
		t.Fatalf("entry %v: books is not a list", entry)
	}
	out := []string{}
	for _, b := range books {
		out = append(out, b.(map[string]any)["rel_path"].(string))
	}
	return out
}

// TestSeriesBooksMatchesBooksPage: each entry is exactly the page
// /books?series=<name>&memberships=1 returns (books in order with the list
// shape, series_list included, next_cursor), and a book in two series through
// more_series is in both entries.
func TestSeriesBooksMatchesBooksPage(t *testing.T) {
	e := newSeriesBooksEnv(t)
	base := e.libPath(e.libID)
	var got seriesBooksBody
	e.get(t, base+"/series/books"+seriesBooksQuery([]string{"Saga", "Spin-off"}, ""), e.adminTok, &got)
	if names := entryNames(got); !reflect.DeepEqual(names, []string{"Saga", "Spin-off"}) {
		t.Fatalf("names = %v", names)
	}
	for _, entry := range got.Series {
		name := entry["name"].(string)
		var want map[string]any
		e.get(t, base+"/books?series="+url.QueryEscape(name)+"&memberships=1", e.adminTok, &want)
		delete(entry, "name")
		if !reflect.DeepEqual(entry, want) {
			t.Errorf("%s entry = %v, want the /books page %v", name, entry, want)
		}
	}
	if paths := entryPaths(t, got.Series[0]); !reflect.DeepEqual(paths, []string{"In/A1", "In/A2", "Inner/C"}) {
		t.Errorf("Saga = %v", paths)
	}
	spin := got.Series[1]
	if paths := entryPaths(t, spin); !reflect.DeepEqual(paths, []string{"In/A2", "Out/B1"}) {
		t.Errorf("Spin-off = %v, want In/A2 (main series Saga) and Out/B1", paths)
	}
	first := spin["books"].([]any)[0].(map[string]any)
	if list, ok := first["series_list"].([]any); !ok || len(list) != 2 {
		t.Errorf("In/A2 series_list = %v, want Saga and Spin-off", first["series_list"])
	}
}

// TestSeriesBooksOrderAndDedupe: entries come back in request order (first
// occurrence), a repeated name once, an empty name not at all, and a series with
// no books as an empty list without a cursor.
func TestSeriesBooksOrderAndDedupe(t *testing.T) {
	e := newSeriesBooksEnv(t)
	q := seriesBooksQuery([]string{"Spin-off", "Nope", "", "Saga", "Spin-off", "Nope"}, "")
	var got seriesBooksBody
	body := e.get(t, e.libPath(e.libID)+"/series/books"+q, e.adminTok, &got)
	if names := entryNames(got); !reflect.DeepEqual(names, []string{"Spin-off", "Nope", "Saga"}) {
		t.Fatalf("names = %v, want Spin-off, Nope, Saga", names)
	}
	if !strings.Contains(body, `{"name":"Nope","books":[]}`) {
		t.Errorf("an empty series should be books:[] without next_cursor: %s", body)
	}
	// Names are verbatim: no case folding or trimming.
	got = seriesBooksBody{}
	e.get(t, e.libPath(e.libID)+"/series/books"+seriesBooksQuery([]string{"saga", " Saga"}, ""), e.adminTok, &got)
	for _, entry := range got.Series {
		if paths := entryPaths(t, entry); len(paths) != 0 {
			t.Errorf("%q matched %v, want none (exact match)", entry["name"], paths)
		}
	}
}

// TestSeriesBooksLimitAndCursor: limit pages each entry as /books does, and the
// next_cursor continues on /books?series=<name>&memberships=1 with that limit;
// an exhausted series omits it.
func TestSeriesBooksLimitAndCursor(t *testing.T) {
	e := newSeriesBooksEnv(t)
	base := e.libPath(e.libID)
	var got seriesBooksBody
	e.get(t, base+"/series/books"+seriesBooksQuery([]string{"Saga", "Other"}, "limit=2"), e.adminTok, &got)
	saga, other := got.Series[0], got.Series[1]
	if paths := entryPaths(t, saga); !reflect.DeepEqual(paths, []string{"In/A1", "In/A2"}) {
		t.Fatalf("Saga page 1 = %v", paths)
	}
	cursor, _ := saga["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("Saga has more books but no next_cursor: %v", saga)
	}
	if _, ok := other["next_cursor"]; ok {
		t.Errorf("Other is exhausted but has a next_cursor: %v", other)
	}
	var want map[string]any
	e.get(t, base+"/books?series=Saga&memberships=1&limit=2", e.adminTok, &want)
	delete(saga, "name")
	if !reflect.DeepEqual(saga, want) {
		t.Errorf("Saga entry = %v, want the /books limit=2 page %v", saga, want)
	}
	var next struct {
		Books      []catalog.Book `json:"books"`
		NextCursor string         `json:"next_cursor"`
	}
	e.get(t, base+"/books?series=Saga&memberships=1&limit=2&cursor="+url.QueryEscape(cursor), e.adminTok, &next)
	if len(next.Books) != 1 || next.Books[0].RelPath != "Inner/C" || next.NextCursor != "" {
		t.Errorf("page 2 = %+v, want Inner/C and no cursor", next)
	}
}

// TestSeriesBooksNames: no (non-empty) names and more than maxSeriesBatch
// distinct ones are 400; exactly maxSeriesBatch, or many repeats of one, is fine.
func TestSeriesBooksNames(t *testing.T) {
	e := newSeriesBooksEnv(t)
	path := e.libPath(e.libID) + "/series/books"
	distinct := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "S" + strconv.Itoa(i)
		}
		return out
	}
	repeats := make([]string, maxSeriesBatch+10)
	for i := range repeats {
		repeats[i] = "Saga"
	}
	for _, c := range []struct {
		q    string
		want int
	}{
		{"", http.StatusBadRequest},
		{"?limit=5", http.StatusBadRequest},
		{seriesBooksQuery([]string{"", ""}, ""), http.StatusBadRequest},
		{seriesBooksQuery(distinct(maxSeriesBatch+1), ""), http.StatusBadRequest},
		{seriesBooksQuery(distinct(maxSeriesBatch), ""), http.StatusOK},
		{seriesBooksQuery(repeats, ""), http.StatusOK},
	} {
		if resp, body := e.do(t, "GET", path+c.q, e.adminTok, ""); resp.StatusCode != c.want {
			t.Errorf("%.60s = %d %s, want %d", c.q, resp.StatusCode, body, c.want)
		}
	}
}

// TestSeriesBooksScoped: a share-scoped member gets only the books inside their
// grant in every entry (allowed: In/A1, In/A2; denied: Inner/C under a sibling
// folder sharing the prefix, and Out/B1 even through more_series), each entry
// still equal to the member's own /books page; the admin sees every book.
func TestSeriesBooksScoped(t *testing.T) {
	e := newSeriesBooksEnv(t)
	base := e.libPath(e.libID)
	names := []string{"Saga", "Spin-off", "Other"}
	var got seriesBooksBody
	e.get(t, base+"/series/books"+seriesBooksQuery(names, ""), e.memberTok, &got)
	want := [][]string{{"In/A1", "In/A2"}, {"In/A2"}, {}}
	for i, entry := range got.Series {
		if paths := entryPaths(t, entry); !reflect.DeepEqual(paths, want[i]) {
			t.Errorf("member %s = %v, want %v", names[i], paths, want[i])
		}
		if len(want[i]) == 0 {
			continue // /books answers an empty page with books:null; this entry is []
		}
		var page map[string]any
		e.get(t, base+"/books?series="+url.QueryEscape(names[i])+"&memberships=1", e.memberTok, &page)
		delete(entry, "name")
		if !reflect.DeepEqual(entry, page) {
			t.Errorf("member %s entry = %v, want the member's /books page %v", names[i], entry, page)
		}
	}
	got = seriesBooksBody{}
	e.get(t, base+"/series/books"+seriesBooksQuery(names, ""), e.adminTok, &got)
	want = [][]string{{"In/A1", "In/A2", "Inner/C"}, {"In/A2", "Out/B1"}, {"Out/B1"}}
	for i, entry := range got.Series {
		if paths := entryPaths(t, entry); !reflect.DeepEqual(paths, want[i]) {
			t.Errorf("admin %s = %v, want %v", names[i], paths, want[i])
		}
	}
}

// TestSeriesBooksAccess: the batch answers access exactly as /books: 401 without
// a session, 403 for a library the caller has no share in (and a non-admin's
// unknown id), 404 for an admin's unknown library, 400 for a bad id.
func TestSeriesBooksAccess(t *testing.T) {
	e := newSeriesBooksEnv(t)
	for _, c := range []struct {
		lib, tok string
		want     int
	}{
		{e.libPath(e.libID), "", http.StatusUnauthorized},
		{e.libPath(e.otherID), e.memberTok, http.StatusForbidden},
		{e.libPath(9999), e.memberTok, http.StatusForbidden},
		{e.libPath(9999), e.adminTok, http.StatusNotFound},
		{"/api/v1/libraries/x", e.adminTok, http.StatusBadRequest},
		{e.libPath(e.otherID), e.adminTok, http.StatusOK},
		{e.libPath(e.libID), e.memberTok, http.StatusOK},
	} {
		books, _ := e.do(t, "GET", c.lib+"/books?series=Saga&memberships=1", c.tok, "")
		batch, body := e.do(t, "GET", c.lib+"/series/books?name=Saga", c.tok, "")
		if batch.StatusCode != c.want || books.StatusCode != c.want {
			t.Errorf("%s tok=%t: series/books = %d (%s), books = %d, want %d", c.lib, c.tok != "", batch.StatusCode, body, books.StatusCode, c.want)
		}
	}
}

// TestSeriesBooksCapability: /server advertises series_books.
func TestSeriesBooksCapability(t *testing.T) {
	e := newTestEnv(t)
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if _, body := e.do(t, "GET", "/api/v1/server", "", ""); json.Unmarshal([]byte(body), &info) != nil || !info.Capabilities["series_books"] {
		t.Fatalf("series_books capability missing: %s", body)
	}
}
