package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// seedCatalog indexes two books in one library and returns its id and the
// library's URL prefix for the admin book endpoints.
func seedCatalog(t *testing.T, e *testEnv) (int64, string) {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []*catalog.Book{
		{LibraryID: lib.ID, RelPath: "Andy Weir/The Martian", IsFolder: true, Title: "The Martian", Author: "Andy Weir",
			Duration: 634 * 60, Format: "m4b", Codec: "aac", AddedAt: "2024-01-01T00:00:00Z",
			Chapters: []metadata.Chapter{{Index: 0, Title: "Sol 6"}, {Index: 1, Title: "Sol 7"}}},
		{LibraryID: lib.ID, RelPath: "Andy Weir/Artemis", IsFolder: true, Title: "Artemis", Author: "Weir, Andy",
			Duration: 540 * 60, Format: "mp3", Codec: "mp3", AddedAt: "2024-02-01T00:00:00Z"},
	} {
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	return lib.ID, "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10)
}

// TestAdminCatalogEndpointsRequireAdmin: every new admin catalog endpoint refuses
// a signed-in member (403) and an anonymous caller (401), and answers an admin.
func TestAdminCatalogEndpointsRequireAdmin(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	book := "?path=" + escape("Andy Weir/The Martian")
	bulk := `{"books":[{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"Andy Weir/Artemis"}],"set":{"narrator":"Rosario Dawson"}}`
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/admin/books", "", 200},
		{"GET", "/api/v1/admin/books/facets", "", 200},
		{"POST", "/api/v1/admin/books/bulk", bulk, 200},
		{"POST", "/api/v1/admin/books/works", `{"books":[{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"Andy Weir/Artemis"}]}`, 200},
		{"GET", "/api/v1/admin/authors", "", 200},
		{"GET", "/api/v1/admin/narrators", "", 200},
		{"GET", "/api/v1/admin/series", "", 200},
		{"GET", base + "/book" + book, "", 200},
		{"PATCH", base + "/book" + book, `{"set":{"title":"The Martian (Classroom Edition)"}}`, 200},
		{"GET", base + "/book/match" + book, "", 200},
		{"PUT", base + "/cover" + book, "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32), 200},
		{"DELETE", base + "/cover" + book, "", 200},
	} {
		if resp, _ := e.do(t, tc.method, tc.path, "", tc.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, tc.method, tc.path, memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		if resp, body := e.do(t, tc.method, tc.path, adminTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("admin %s %s = %d %s, want %d", tc.method, tc.path, resp.StatusCode, body, tc.want)
		}
	}
	// The member's refused bulk edit and PATCH changed nothing; the admin's did.
	got, _ := e.cat.GetBookByPath(context.Background(), libID, "Andy Weir/Artemis")
	if got.Narrator != "Rosario Dawson" {
		t.Fatalf("admin bulk edit not applied: %+v", got)
	}
}

func TestAdminListBooksAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)

	var page struct {
		Books []struct {
			P   string `json:"path"`
			Lib string `json:"library_name"`
		} `json:"books"`
		NextCursor string `json:"next_cursor"`
	}
	resp, body := e.do(t, "GET", "/api/v1/admin/books?sort=added&order=desc&limit=1&library_id="+strconv.FormatInt(libID, 10), adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 1 || page.Books[0].P != "Andy Weir/Artemis" || page.Books[0].Lib != "Main" || page.NextCursor == "" {
		t.Fatalf("first page = %s", body)
	}
	if strings.Contains(body, `"id"`) {
		t.Fatalf("the admin list must not expose the internal book id: %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/books?sort=added&order=desc&limit=1&cursor="+page.NextCursor, adminTok, "")
	if !strings.Contains(body, `"path":"Andy Weir/The Martian"`) || !strings.Contains(body, `"chapter_count":2`) {
		t.Fatalf("second page = %s", body)
	}

	// Unparseable filters are refused, never silently dropped.
	for _, q := range []string{
		strings.Repeat("format=m4b&", maxFilterValues+1),
		"has_cover=yes", "library_id=x", "min_duration=-1", "added_after=yesterday",
		"order=sideways", "sort=rel_path", "cursor=!!!",
	} {
		if resp, body := e.do(t, "GET", "/api/v1/admin/books?"+q, adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s = %d %s, want 400", q, resp.StatusCode, body)
		}
	}
	// A bound with a UTC offset means the same instant as its UTC form: The Martian
	// was added 2024-01-01T00:00:00Z, which is 02:00 at +02:00.
	for bound, want := range map[string]bool{
		"2024-01-01T02:00:00%2B02:00": true,  // = 00:00Z: inclusive, so included
		"2024-01-01T02:00:01%2B02:00": false, // one second later: excluded
	} {
		_, body := e.do(t, "GET", "/api/v1/admin/books?q=martian&added_after="+bound, adminTok, "")
		if got := strings.Contains(body, "The Martian"); got != want {
			t.Errorf("added_after=%s: included=%v, want %v (%s)", bound, got, want, body)
		}
	}
	if resp, body := e.do(t, "GET", "/api/v1/admin/books/facets?format=mp3", adminTok, ""); resp.StatusCode != 200 ||
		!strings.Contains(body, `"total":1`) || !strings.Contains(body, `{"value":"m4b","count":1}`) {
		t.Fatalf("facets = %d %s", resp.StatusCode, body)
	}
}

func TestAdminAggregatesAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	seedCatalog(t, e)
	_, body := e.do(t, "GET", "/api/v1/admin/authors", adminTok, "")
	if !strings.Contains(body, `"authors":[{"name":"Andy Weir","books":1`) ||
		!strings.Contains(body, `"merge_suggestions":[{"names":["Andy Weir","Weir, Andy"],"suggested":"Andy Weir","books":2,"other_books":1}]`) {
		t.Fatalf("authors = %s", body)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/narrators", adminTok, ""); !strings.Contains(body, `"narrators":[]`) || !strings.Contains(body, `"unknown":2`) {
		t.Fatalf("narrators = %s", body)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/series", adminTok, ""); body != "{\"series\":[]}\n" {
		t.Fatalf("series = %q", body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/authors?library_id=0", adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("library_id=0 = %d, want 400", resp.StatusCode)
	}
}

func TestAdminEditBookAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	url := base + "/book?path=" + escape("Andy Weir/The Martian")

	resp, body := e.do(t, "PATCH", url, adminTok,
		`{"set":{"title":"The Martian: Classroom Edition","series":"Mars","series_index":"1"},"chapters":{"set":{"1":"Sol 7: Hab"}}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	var d struct {
		Book     struct{ Title string } `json:"book"`
		Fields   map[string]catalog.FieldValue
		Chapters []catalog.AdminChapter
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Book.Title != "The Martian: Classroom Edition" || d.Fields["title"].Source != "edited" ||
		d.Fields["title"].Scanned != "The Martian" || d.Fields["title"].EditedBy != "admin" || !d.Chapters[1].Edited {
		t.Fatalf("patch response = %s", body)
	}
	// The player-facing item sees the edit (players get better values, same shape).
	_, item := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/item?path="+escape("Andy Weir/The Martian"), adminTok, "")
	if !strings.Contains(item, `"title":"The Martian: Classroom Edition"`) || !strings.Contains(item, `"title":"Sol 7: Hab"`) ||
		strings.Contains(item, `"published"`) || strings.Contains(item, `"scanned"`) {
		t.Fatalf("item after edit = %s", item)
	}

	for name, tc := range map[string]struct {
		body, code, field string
		want              int
	}{
		"empty":         {`{}`, "", "", 400},
		"unknown key":   {`{"set":{"title":"x"},"extra":1}`, "", "", 400},
		"bad field":     {`{"set":{"cover_path":"/etc/passwd"}}`, codeInvalidOverride, "cover_path", 400},
		"bad value":     {`{"set":{"asin":"nope"}}`, codeInvalidOverride, "asin", 400},
		"bad chapter":   {`{"chapters":{"set":{"7":"x"}}}`, codeInvalidOverride, "chapters", 400},
		"revert unknow": {`{"revert":["rel_path"]}`, codeInvalidOverride, "rel_path", 400},
	} {
		resp, body := e.do(t, "PATCH", url, adminTok, tc.body)
		var env struct{ Code, Field string }
		_ = json.Unmarshal([]byte(body), &env)
		if resp.StatusCode != tc.want || env.Code != tc.code || env.Field != tc.field {
			t.Errorf("%s: %d %s, want %d code=%q field=%q", name, resp.StatusCode, body, tc.want, tc.code, tc.field)
		}
	}
	// Revert goes back to what the scan found.
	if _, body := e.do(t, "PATCH", url, adminTok, `{"revert":["title"],"chapters":{"revert":[1]}}`); !strings.Contains(body, `"title":"The Martian"`) {
		t.Fatalf("revert = %s", body)
	}

	// Unknown library, missing path, unindexed path.
	for path, want := range map[string]int{
		"/api/v1/admin/libraries/999/book?path=x": 404,
		base + "/book":                             400,
		base + "/book?path=" + escape("../"):       400,
		base + "/book?path=" + escape("Nope/Nope"): 404,
	} {
		if resp, body := e.do(t, "GET", path, adminTok, ""); resp.StatusCode != want {
			t.Errorf("GET %s = %d %s, want %d", path, resp.StatusCode, body, want)
		}
	}
}

// The book page names the match dialog's search text: the title and author, or
// the folders' when the tags are swapped.
func TestAdminBookMatchQuery(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: libID,
		RelPath: "Andy Weir/Project Hail Mary", IsFolder: true, Title: "Andy Weir", Author: "Project Hail Mary",
		AddedAt: "2024-03-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"Andy Weir/The Martian":       "The Martian Andy Weir",
		"Andy Weir/Project Hail Mary": "Project Hail Mary Andy Weir",
	} {
		resp, body := e.do(t, "GET", base+"/book?path="+escape(path), adminTok, "")
		var d struct {
			MatchQuery string `json:"match_query"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil || resp.StatusCode != 200 || d.MatchQuery != want {
			t.Errorf("%s: %d match_query = %q, want %q", path, resp.StatusCode, d.MatchQuery, want)
		}
	}
}

func TestAdminBulkEditAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	lib := strconv.FormatInt(libID, 10)
	both := `[{"library_id":` + lib + `,"path":"Andy Weir/The Martian"},{"library_id":` + lib + `,"path":"Andy Weir/Artemis"}]`
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, `{"books":`+both+`,"set":{"author":"Andy Weir"}}`); resp.StatusCode != 200 || body != "{\"updated\":2}\n" {
		t.Fatalf("bulk = %d %s", resp.StatusCode, body)
	}
	// A path named twice (once non-canonically) is one book updated.
	twice := `[{"library_id":` + lib + `,"path":"Andy Weir/Artemis"},{"library_id":` + lib + `,"path":"Andy Weir/./Artemis"}]`
	if _, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, `{"books":`+twice+`,"set":{"author":"Andy Weir"}}`); body != "{\"updated\":1}\n" {
		t.Fatalf("bulk with a duplicate = %s, want 1 updated", body)
	}
	_, body := e.do(t, "GET", "/api/v1/admin/authors", adminTok, "")
	if !strings.Contains(body, `"authors":[{"name":"Andy Weir","books":2`) || !strings.Contains(body, `"merge_suggestions":[]`) {
		t.Fatalf("authors after merge = %s", body)
	}
	missing := `[{"library_id":` + lib + `,"path":"Andy Weir/Artemis"},{"library_id":` + lib + `,"path":"Gone"}]`
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"missing book": {`{"books":` + missing + `,"set":{"narrator":"x"}}`, 404},
		"no books":     {`{"books":[],"set":{"narrator":"x"}}`, 400},
		"nothing":      {`{"books":` + both + `}`, 400},
		"chapters":     {`{"books":` + both + `,"chapters":{"set":{"0":"x"}}}`, 400},
		"bad value":    {`{"books":` + both + `,"set":{"series_index":"x"}}`, 400},
	} {
		if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, tc.want)
		}
	}
	if b, _ := e.cat.GetBookByPath(context.Background(), libID, "Andy Weir/Artemis"); b.Narrator != "" {
		t.Fatalf("a refused bulk edit changed a book: %+v", b)
	}
	var many bytes.Buffer
	many.WriteString(`{"set":{"narrator":"x"},"books":[`)
	for i := range maxBulkBooks + 1 {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"library_id":1,"path":"p"}`)
	}
	many.WriteString(`]}`)
	if resp, _ := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, many.String()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized bulk = %d, want 400", resp.StatusCode)
	}
}

// TestAdminMatchSendsTheBookFacts: against a metaserve with the structured
// match, the handler hands it the book's tags AND its library path, and the
// console gets metaserve's score and reasons in the unchanged envelope.
func TestAdminMatchSendsTheBookFacts(t *testing.T) {
	t.Parallel()
	m := &mockMetaserve{lookupCode: http.StatusNotFound, match: true}
	e := newMetaEnvMock(t, true, m)
	adminTok, _, _ := adminAndMember(t, e)
	_, base := seedCatalog(t, e)
	_, body := e.do(t, "GET", base+"/book/match?path="+escape("Andy Weir/Artemis")+"&q=artemis", adminTok, "")
	var out struct {
		Candidates []struct {
			WorkID  string `json:"work_id"`
			Score   int
			Reasons struct {
				Title  float64
				Author string
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].Score != 88 || out.Candidates[0].Reasons.Author != "full" {
		t.Fatalf("match = %s", body)
	}
	m.mu.Lock()
	got := m.gotMatch
	m.mu.Unlock()
	// The path's author folder and the tagged inverted credit both go up; the
	// title is the path's and the tag's (deduplicated).
	if got.Get("q") != "artemis" || strings.Join(got["author"], "|") != "Andy Weir|Weir, Andy" ||
		strings.Join(got["title"], "|") != "Artemis" || got.Get("runtime") != "32400" {
		t.Fatalf("works/match sent %v", got)
	}
}

func TestAdminMatchAPI(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, _, _ := adminAndMember(t, e)
	_, base := seedCatalog(t, e)
	url := base + "/book/match?path=" + escape("Andy Weir/The Martian")

	_, body := e.do(t, "GET", url, adminTok, "")
	var out struct {
		Candidates []struct {
			WorkID     string `json:"work_id"`
			Score      int
			CoverURL   string `json:"cover_url"`
			Recordings []struct{ ASINs []string }
		}
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].WorkID != "the-martian" || out.Candidates[0].Score != 100 ||
		out.Candidates[0].CoverURL != "https://c/w.jpg" || len(out.Candidates[0].Recordings[0].ASINs) != 1 {
		t.Fatalf("match = %s", body)
	}
	if _, body := e.do(t, "GET", url+"&q=nothing", adminTok, ""); body != "{\"candidates\":[]}\n" {
		t.Fatalf("no hits = %q", body)
	}
	if resp, _ := e.do(t, "GET", url+"&q=down", adminTok, ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down = %d, want 502", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", url+"&q="+strings.Repeat("a", maxMatchQuery+1), adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long query = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", base+"/book/match?path=Nope", adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unindexed = %d, want 404", resp.StatusCode)
	}

	off := newMetaEnv(t, false, 0)
	offTok, _, _ := adminAndMember(t, off)
	_, offBase := seedCatalog(t, off)
	resp, body := off.do(t, "GET", offBase+"/book/match?path="+escape("Andy Weir/The Martian"), offTok, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, codeMetadataOff) {
		t.Fatalf("metadata off = %d %s", resp.StatusCode, body)
	}
}

// TestAdminBookWorksAPI: each book's community work id, in request order, for the
// Series screen; books with no identifier or no indexed book answer "" and are not
// failed, a down upstream marks only the books it couldn't look up as failed, and
// metadata off is a 404 like the match.
func TestAdminBookWorksAPI(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	ctx := context.Background()
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: libID, RelPath: "Andy Weir/Project Hail Mary",
		IsFolder: true, Title: "Project Hail Mary", Author: "Andy Weir", ASIN: "B08GB58KD5", AddedAt: "2024-03-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	lib := strconv.FormatInt(libID, 10)
	body := `{"books":[` +
		`{"library_id":` + lib + `,"path":"Andy Weir/Project Hail Mary/"},` + // trailing slash: still the book
		`{"library_id":` + lib + `,"path":"Andy Weir/Artemis"},` + // no ASIN/ISBN
		`{"library_id":` + lib + `,"path":"Andy Weir/Nope"},` + // no book indexed there
		`{"library_id":999,"path":"Andy Weir/Project Hail Mary"}]}` // no such library
	resp, got := e.do(t, "POST", "/api/v1/admin/books/works", adminTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("works = %d %s", resp.StatusCode, got)
	}
	var out struct {
		Works []bookWork `json:"works"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(out.Works))
	for i, w := range out.Works {
		ids[i] = w.WorkID
	}
	if !slices.Equal(ids, []string{"the-martian", "", "", ""}) || strings.Contains(got, `"failed":true`) ||
		out.Works[0].LibraryID != libID || out.Works[0].Path != "Andy Weir/Project Hail Mary/" || out.Works[3].LibraryID != 999 {
		t.Fatalf("works = %s", got)
	}

	for name, tc := range map[string]struct{ body, code string }{
		"empty":    {`{"books":[]}`, ""},
		"bad json": {`{"books":`, ""},
		"too many": {`{"books":[` + strings.Repeat(`{"library_id":1,"path":"x"},`, maxWorkBooks) + `{"library_id":1,"path":"x"}]}`, codeTooLarge},
	} {
		resp, got := e.do(t, "POST", "/api/v1/admin/books/works", adminTok, tc.body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, tc.code) {
			t.Errorf("%s = %d %s, want 400 %s", name, resp.StatusCode, got, tc.code)
		}
	}

	down := newMetaEnv(t, true, http.StatusInternalServerError)
	downTok, _, _ := adminAndMember(t, down)
	downLib := seedBook(t, down, "Author/Book", "B0DOWN")
	downRef := `{"library_id":` + strconv.FormatInt(downLib, 10) + `,"path":`
	_, got = down.do(t, "POST", "/api/v1/admin/books/works", downTok,
		`{"books":[`+downRef+`"Author/Book"},`+downRef+`"Author/None"}]}`) // the second has no book: no lookup
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Works) != 2 || out.Works[0].WorkID != "" || !out.Works[0].Failed || out.Works[1].Failed {
		t.Fatalf("upstream down = %s, want only the looked-up book failed", got)
	}

	off := newMetaEnv(t, false, 0)
	offTok, _, _ := adminAndMember(t, off)
	resp, got = off.do(t, "POST", "/api/v1/admin/books/works", offTok, `{"books":[{"library_id":1,"path":"x"}]}`)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(got, codeMetadataOff) {
		t.Fatalf("metadata off = %d %s", resp.StatusCode, got)
	}
}

// TestCustomCoverAPI: an uploaded cover is served by the ordinary cover endpoint
// ahead of the book's own art, still behind the caller's scope; bad uploads are
// refused.
func TestCustomCoverAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	path := "?path=" + escape("Andy Weir/The Martian")
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	if resp, body := e.do(t, "PUT", base+"/cover"+path, adminTok, png); resp.StatusCode != 200 {
		t.Fatalf("upload = %d %s", resp.StatusCode, body)
	}
	cover := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/cover" + path
	resp, body := e.do(t, "GET", cover, adminTok, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || body != png ||
		resp.Header.Get("Cache-Control") != "private, no-cache" || resp.Header.Get("ETag") == "" ||
		resp.Header.Get("Last-Modified") != "" {
		t.Fatalf("custom cover = %d %v", resp.StatusCode, resp.Header)
	}
	// A fresh conditional request is a 304 (answered without reading the image).
	req, _ := http.NewRequest("GET", e.srv.URL+cover, nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if cond, err := http.DefaultClient.Do(req); err != nil || cond.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional custom cover = %v %v, want 304", cond, err)
	} else {
		cond.Body.Close()
	}
	// A member with no access to the library is refused even though a custom cover
	// exists (the cover sits behind authorizedPath like everything else).
	if resp, _ := e.do(t, "GET", cover, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope custom cover = %d, want 403", resp.StatusCode)
	}
	if _, body := e.do(t, "GET", base+"/book"+path, adminTok, ""); !strings.Contains(body, `"custom_cover":true`) || !strings.Contains(body, `"has_cover":true`) {
		t.Fatalf("detail after upload = %s", body)
	}

	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"not an image": {path, "<svg onload=alert(1)>", http.StatusUnsupportedMediaType},
		"too big":      {path, "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", catalog.MaxCoverBytes), http.StatusRequestEntityTooLarge},
		"unindexed":    {"?path=Nope", png, http.StatusNotFound},
	} {
		if resp, body := e.do(t, "PUT", base+"/cover"+tc.path, adminTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, tc.want)
		}
	}
	if resp, _ := e.do(t, "DELETE", base+"/cover"+path, adminTok, ""); resp.StatusCode != 200 {
		t.Fatal("delete cover failed")
	}
	if resp, _ := e.do(t, "GET", cover, adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete the book's own (absent) art = %d, want 404", resp.StatusCode)
	}
}

// TestCustomCoverFollowsTheBook: a custom cover is the book's whichever path the
// cover is asked for by (a part resolves to its folder book, as /item does), and
// only while the book is there. A sidecar image is served with a bounded lifetime,
// so a custom cover set later shows within a day rather than by heuristic.
func TestCustomCoverFollowsTheBook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	root := t.TempDir()
	fixtures, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	for _, rel := range []string{
		"Will Wight/Cradle/01 - Unsouled.m4b", "Will Wight/Cradle/02 - Soulsmith.m4b",
		"Brandon Sanderson/Mistborn/01 - The Final Empire.m4b", // keeps the library non-empty once Cradle goes
	} {
		data, err := os.ReadFile(filepath.Join(fixtures, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jpeg := "\xff\xd8\xff\xe0\x00\x10JFIF\x00" + strings.Repeat("\x00", 16)
	if err := os.WriteFile(filepath.Join(root, "Will Wight", "Cradle", "cover.jpg"), []byte(jpeg), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	scanner := library.NewScanner(e.cat, "", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(lib.ID, 10)
	cover := func(p string) string { return "/api/v1/libraries/" + id + "/cover?path=" + escape(p) }

	if resp, body := e.do(t, "GET", cover("Will Wight/Cradle"), adminTok, ""); resp.StatusCode != 200 || body != jpeg ||
		resp.Header.Get("Cache-Control") != "private, max-age=86400" {
		t.Fatalf("sidecar cover = %d %v", resp.StatusCode, resp.Header)
	}

	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	if resp, body := e.do(t, "PUT", "/api/v1/admin/libraries/"+id+"/cover?path="+escape("Will Wight/Cradle"), adminTok, png); resp.StatusCode != 200 {
		t.Fatalf("upload = %d %s", resp.StatusCode, body)
	}
	// An upload is the admin's own: clearing the community matches keeps it.
	if cleared, err := e.cat.ClearCommunityMatches(ctx, lib.ID); err != nil || cleared.Covers != 0 {
		t.Fatalf("clear = %+v %v, want the upload kept", cleared, err)
	}
	for _, p := range []string{"Will Wight/Cradle", "Will Wight/Cradle/01 - Unsouled.m4b"} {
		if resp, body := e.do(t, "GET", cover(p), adminTok, ""); resp.StatusCode != 200 || body != png {
			t.Errorf("cover by %q = %d %s, want the custom cover", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}

	// Gone from disk and pruned: no cover at all, custom or not.
	if err := os.RemoveAll(filepath.Join(root, "Will Wight")); err != nil {
		t.Fatal(err)
	}
	if res, err := scanner.Scan(ctx, *lib); err != nil || res.Removed != 1 {
		t.Fatalf("rescan: %+v %v", res, err)
	}
	if resp, _ := e.do(t, "GET", cover("Will Wight/Cradle"), adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cover of a pruned book = %d, want 404", resp.StatusCode)
	}
}
