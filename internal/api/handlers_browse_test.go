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

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// browseEnv is a library with books inside and outside a share grant ("In"), a
// member holding that grant, an admin, and a second library the member can't
// reach.
type browseEnv struct {
	*testEnv
	libID, otherID      int64
	adminTok, memberTok string
}

func (e *browseEnv) libPath(id int64) string { return "/api/v1/libraries/" + strconv.FormatInt(id, 10) }

func newBrowseEnv(t *testing.T) *browseEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	other, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Other", Root: t.TempDir()})
	for _, b := range []*catalog.Book{
		{RelPath: "In/A1", Title: "Saga One", Author: "Ann", Narrator: "Nora", Series: "Saga", SeriesIndex: 1, Duration: 100},
		{RelPath: "In/A2", Title: "Saga Two", Author: "Ann", Narrator: "Ned", Series: "Saga", SeriesIndex: 2, Duration: 200},
		{RelPath: "In/Blank", Title: "Untitled", Duration: 50},
		// Outside the grant: a sibling sharing the "In" prefix, a book by the same
		// author in the same series, and people and a series found nowhere inside.
		{RelPath: "Inner/C", Title: "Saga Extra", Author: "Ann", Narrator: "Nora", Series: "Saga", SeriesIndex: 3, Duration: 400},
		{RelPath: "Out/B1", Title: "Other One", Author: "Bob", Narrator: "Nora", Series: "Other", SeriesIndex: 1, Duration: 300},
	} {
		b.LibraryID, b.Format = lib.ID, "m4b"
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: other.ID, RelPath: "X", Title: "X", Author: "Xena", Format: "m4b"}); err != nil {
		t.Fatal(err)
	}
	adminTok, memberTok, memberID := adminAndMember(t, e)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "In only"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "In"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	return &browseEnv{testEnv: e, libID: lib.ID, otherID: other.ID, adminTok: adminTok, memberTok: memberTok}
}

// get fetches path and decodes a 200 body into out, returning the raw body.
func (e *browseEnv) get(t *testing.T, path, tok string, out any) string {
	t.Helper()
	resp, body := e.do(t, "GET", path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("GET %s: %v (%s)", path, err, body)
	}
	return body
}

type peopleBody struct {
	Authors   []catalog.PersonCount `json:"authors"`
	Narrators []catalog.PersonCount `json:"narrators"`
	Unknown   int                   `json:"unknown"`
}

// TestBrowsePeopleScoped: a share-scoped member's authors/narrators count only
// the books inside their grant (denied: a book outside it, even one under a
// sibling folder sharing the prefix, contributes nothing), and an admin's count
// every book (allowed). No merge suggestions on the player route.
func TestBrowsePeopleScoped(t *testing.T) {
	e := newBrowseEnv(t)
	base := e.libPath(e.libID)

	var got peopleBody
	body := e.get(t, base+"/authors", e.memberTok, &got)
	if want := []catalog.PersonCount{{Name: "Ann", Books: 2, Duration: 300}}; !reflect.DeepEqual(got.Authors, want) || got.Unknown != 1 {
		t.Fatalf("member authors = %+v unknown=%d, want %+v unknown=1", got.Authors, got.Unknown, want)
	}
	if strings.Contains(body, "merge_suggestions") || strings.Contains(body, "Bob") {
		t.Fatalf("member authors leaked admin data or an out-of-grant author: %s", body)
	}
	got = peopleBody{}
	e.get(t, base+"/narrators", e.memberTok, &got)
	if want := []catalog.PersonCount{{Name: "Ned", Books: 1, Duration: 200}, {Name: "Nora", Books: 1, Duration: 100}}; !reflect.DeepEqual(got.Narrators, want) || got.Unknown != 1 {
		t.Fatalf("member narrators = %+v unknown=%d", got.Narrators, got.Unknown)
	}

	got = peopleBody{}
	e.get(t, base+"/authors", e.adminTok, &got)
	if want := []catalog.PersonCount{{Name: "Ann", Books: 3, Duration: 700}, {Name: "Bob", Books: 1, Duration: 300}}; !reflect.DeepEqual(got.Authors, want) || got.Unknown != 1 {
		t.Fatalf("admin authors = %+v unknown=%d", got.Authors, got.Unknown)
	}
	got = peopleBody{}
	e.get(t, base+"/narrators", e.adminTok, &got)
	if want := []catalog.PersonCount{{Name: "Ned", Books: 1, Duration: 200}, {Name: "Nora", Books: 3, Duration: 800}}; !reflect.DeepEqual(got.Narrators, want) {
		t.Fatalf("admin narrators = %+v", got.Narrators)
	}
}

// TestBrowseSeriesScoped: the member's series hold only their granted books'
// counts and positions; the admin's hold every book's.
func TestBrowseSeriesScoped(t *testing.T) {
	e := newBrowseEnv(t)
	base := e.libPath(e.libID)
	var got struct {
		Series []catalog.SeriesCount `json:"series"`
	}
	e.get(t, base+"/series", e.memberTok, &got)
	want := []catalog.SeriesCount{{Name: "Saga", Author: "Ann", Books: 2, Duration: 300, Positions: []float64{1, 2}}}
	if !reflect.DeepEqual(got.Series, want) {
		t.Fatalf("member series = %+v, want %+v", got.Series, want)
	}
	got.Series = nil
	e.get(t, base+"/series", e.adminTok, &got)
	want = []catalog.SeriesCount{
		{Name: "Other", Author: "Bob", Books: 1, Duration: 300, Positions: []float64{1}},
		{Name: "Saga", Author: "Ann", Books: 3, Duration: 700, Positions: []float64{1, 2, 3}},
	}
	if !reflect.DeepEqual(got.Series, want) {
		t.Fatalf("admin series = %+v, want %+v", got.Series, want)
	}
}

// TestBrowseEmptyArrays: a grant holding no books gives empty arrays, never null.
func TestBrowseEmptyArrays(t *testing.T) {
	e := newBrowseEnv(t)
	ctx := context.Background()
	u, _ := e.auth.CreateUser(ctx, "empty", "empty-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Nothing"})
	_ = e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: e.libID, Path: "Nowhere"})
	_ = e.cat.GrantShare(ctx, u.ID, share.ID)
	tok, _ := e.auth.IssueToken(ctx, u.ID, auth.KindSession, "t", 0)
	base := e.libPath(e.libID)
	for path, want := range map[string]string{
		"/authors":   `{"authors":[],"unknown":0}`,
		"/narrators": `{"narrators":[],"unknown":0}`,
		"/series":    `{"series":[]}`,
	} {
		if resp, body := e.do(t, "GET", base+path, tok, ""); resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != want {
			t.Errorf("%s = %d %s, want %s", path, resp.StatusCode, body, want)
		}
	}
}

// TestBrowseAccess: the browse lists need a session, refuse a library the caller
// has no share in (403, also for an unknown id: it says nothing about which ids
// exist) and answer an admin's unknown library with 404.
func TestBrowseAccess(t *testing.T) {
	e := newBrowseEnv(t)
	for _, p := range []string{"/authors", "/narrators", "/series"} {
		if resp, _ := e.do(t, "GET", e.libPath(e.libID)+p, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", p, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", e.libPath(e.otherID)+p, e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member, no-access library %s = %d, want 403", p, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", e.libPath(9999)+p, e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member, unknown library %s = %d, want 403", p, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", e.libPath(9999)+p, e.adminTok, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("admin, unknown library %s = %d, want 404", p, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", "/api/v1/libraries/x"+p, e.adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("bad library id %s = %d, want 400", p, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", e.libPath(e.otherID)+p, e.adminTok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("admin, other library %s = %d, want 200", p, resp.StatusCode)
		}
	}
}

// TestListBooksNarratorFilter: ?narrator= is an exact match, within the caller's
// scope like the other filters.
func TestListBooksNarratorFilter(t *testing.T) {
	e := newBrowseEnv(t)
	base := e.libPath(e.libID)
	paths := func(tok, q string) []string {
		var page struct {
			Books []catalog.Book `json:"books"`
		}
		e.get(t, base+"/books?sort=title&"+q, tok, &page)
		out := []string{}
		for _, b := range page.Books {
			out = append(out, b.RelPath)
		}
		return out
	}
	if got, want := paths(e.memberTok, "narrator=Nora"), []string{"In/A1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("member narrator=Nora = %v, want %v", got, want)
	}
	if got, want := paths(e.adminTok, "narrator=Nora"), []string{"Out/B1", "Inner/C", "In/A1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("admin narrator=Nora = %v, want %v", got, want)
	}
	if got := paths(e.adminTok, "narrator=nora"); len(got) != 0 {
		t.Errorf("narrator is an exact match, got %v", got)
	}
	if got, want := paths(e.adminTok, "narrator=Nora&author=Ann"), []string{"Inner/C", "In/A1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("narrator+author = %v, want %v", got, want)
	}
}

// TestBookPublishedAndDescription: published rides on every player book;
// description only on the item endpoint, never on a list page (books, search,
// recent).
func TestBookPublishedAndDescription(t *testing.T) {
	e := newBrowseEnv(t)
	base := e.libPath(e.libID)
	edit := `{"set":{"published":"2011-03","description":"A long blurb about the saga."}}`
	adminBase := "/api/v1/admin/libraries/" + strconv.FormatInt(e.libID, 10)
	if resp, body := e.do(t, "PATCH", adminBase+"/book?path="+url.QueryEscape("In/A1"), e.adminTok, edit); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit = %d %s", resp.StatusCode, body)
	}

	var item map[string]any
	e.get(t, base+"/item?path="+url.QueryEscape("In/A1"), e.memberTok, &item)
	if item["published"] != "2011-03" || item["description"] != "A long blurb about the saga." {
		t.Fatalf("item published/description = %v / %v", item["published"], item["description"])
	}
	if item["rel_path"] != "In/A1" || item["direct_playable"] == nil {
		t.Fatalf("item lost its book shape: %v", item)
	}
	// A book with neither leaves both out.
	item = nil
	e.get(t, base+"/item?path="+url.QueryEscape("In/A2"), e.memberTok, &item)
	if _, ok := item["published"]; ok {
		t.Errorf("unset published should be omitted: %v", item)
	}
	if _, ok := item["description"]; ok {
		t.Errorf("unset description should be omitted: %v", item)
	}

	for _, path := range []string{base + "/books", "/api/v1/search?q=saga", "/api/v1/books/recent"} {
		var page struct {
			Books []map[string]any `json:"books"`
		}
		body := e.get(t, path, e.memberTok, &page)
		if strings.Contains(body, `"description"`) {
			t.Errorf("%s carries a description: %s", path, body)
		}
		found := false
		for _, b := range page.Books {
			if b["rel_path"] == "In/A1" {
				found = true
				if b["published"] != "2011-03" {
					t.Errorf("%s: In/A1 published = %v, want 2011-03", path, b["published"])
				}
			}
		}
		if !found {
			t.Errorf("%s: In/A1 missing: %s", path, body)
		}
	}
}

// TestBrowsePeopleCapability: /server advertises the browse lists.
func TestBrowsePeopleCapability(t *testing.T) {
	e := newTestEnv(t)
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	resp, body := e.do(t, "GET", "/api/v1/server", "", "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil || !info.Capabilities["browse_people"] {
		t.Fatalf("browse_people capability missing: %d %s", resp.StatusCode, body)
	}
}
