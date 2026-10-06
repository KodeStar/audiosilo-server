package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// wireCollection is a Collection as the wire carries it; SharedWith stays raw so
// a test can tell absent from [].
type wireCollection struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       struct{ ID int64 }
	Owned       bool            `json:"owned"`
	SharedWith  json.RawMessage `json:"shared_with"`
	ItemCount   int             `json:"item_count"`
	Preview     []struct {
		RelPath string `json:"rel_path"`
	} `json:"preview"`
	UpdatedAt string `json:"updated_at"`
}

type collectionDetail struct {
	Collection wireCollection `json:"collection"`
	Items      []wireItem     `json:"items"`
}

// colURL is /me/collections/{id} plus a suffix.
func colURL(id int64, suffix string) string {
	return "/api/v1/me/collections/" + strconv.FormatInt(id, 10) + suffix
}

// createCollection POSTs a collection as tok, failing on anything but 201.
func (l *listsEnv) createCollection(t *testing.T, tok, name string) wireCollection {
	t.Helper()
	resp, body := l.do(t, "POST", "/api/v1/me/collections", tok, fmt.Sprintf(`{"name":%q}`, name))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %q = %d %s", name, resp.StatusCode, body)
	}
	var out struct {
		Collection wireCollection `json:"collection"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Collection
}

// detail GETs a collection as tok, failing on anything but 200.
func (l *listsEnv) detail(t *testing.T, tok string, id int64) (collectionDetail, string) {
	t.Helper()
	resp, body := l.do(t, "GET", colURL(id, ""), tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET collection = %d %s", resp.StatusCode, body)
	}
	var out collectionDetail
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out, body
}

// Create, add (a part path stores its book), replace, read, rename, describe,
// delete: the owner's whole lifecycle, and the request errors.
func TestCollectionsLifecycle(t *testing.T) {
	l := newListsEnv(t)
	col := l.createCollection(t, l.oliveTok, "  Bedtime  ")
	if col.Name != "Bedtime" || !col.Owned || string(col.SharedWith) != "[]" || col.ItemCount != 0 ||
		col.Preview == nil || col.Owner.ID != l.olive {
		t.Fatalf("created = %+v", col)
	}

	resp, body := l.do(t, "POST", colURL(col.ID, "/items"), l.oliveTok, l.addJSON(cradlePart, -1))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"items":[`) {
		t.Fatalf("add item = %d %s", resp.StatusCode, body)
	}
	l.do(t, "POST", colURL(col.ID, "/items"), l.oliveTok, l.addJSON(mistbornBook, 0))
	got, _ := l.detail(t, l.oliveTok, col.ID)
	if itemPathsOf(got.Items) != mistbornBook+"|"+cradleBook || got.Collection.ItemCount != 2 || len(got.Collection.Preview) != 2 {
		t.Fatalf("detail = %+v", got)
	}

	resp, body = l.do(t, "PUT", colURL(col.ID, "/items"), l.oliveTok, l.itemsJSON(thread, "Will Wight/Nope", cradleBook, thread))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace = %d %s", resp.StatusCode, body)
	}
	got, _ = l.detail(t, l.oliveTok, col.ID)
	if itemPathsOf(got.Items) != thread+"|"+cradleBook {
		t.Fatalf("after replace = %s", itemPathsOf(got.Items))
	}
	for range 2 {
		if resp, _ := l.do(t, "DELETE", colURL(col.ID, "/items")+l.removeQuery(thread), l.oliveTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("remove item = %d", resp.StatusCode)
		}
	}
	got, _ = l.detail(t, l.oliveTok, col.ID)
	if itemPathsOf(got.Items) != cradleBook {
		t.Fatalf("after remove = %s", itemPathsOf(got.Items))
	}

	before := got.Collection.UpdatedAt
	resp, body = l.do(t, "PATCH", colURL(col.ID, ""), l.oliveTok, `{"name":"Stories","description":"For the kids"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"Stories"`) || !strings.Contains(body, `"description":"For the kids"`) {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	got, _ = l.detail(t, l.oliveTok, col.ID)
	if got.Collection.UpdatedAt == before {
		t.Fatal("a rename did not move updated_at")
	}

	for name, tc := range map[string]struct {
		method, url, body string
		want              int
	}{
		"create empty name":    {"POST", "/api/v1/me/collections", `{"name":"  "}`, 400},
		"create control char":  {"POST", "/api/v1/me/collections", `{"name":"a\u0007b"}`, 400},
		"create unknown field": {"POST", "/api/v1/me/collections", `{"name":"x","color":"red"}`, 400},
		"patch long name":      {"PATCH", colURL(col.ID, ""), fmt.Sprintf(`{"name":%q}`, strings.Repeat("n", 101)), 400},
		"bad id":               {"GET", "/api/v1/me/collections/abc", "", 400},
		"add not a book":       {"POST", colURL(col.ID, "/items"), l.addJSON("Will Wight/Nope.m4b", -1), 404},
		"add negative":         {"POST", colURL(col.ID, "/items"), fmt.Sprintf(`{"library_id":%d,"path":%q,"position":-2}`, l.libID, mistbornBook), 400},
		"remove no path":       {"DELETE", colURL(col.ID, "/items?library_id=1"), "", 400},
		"unknown collection":   {"GET", colURL(9999, ""), "", 404},
	} {
		if resp, b := l.do(t, tc.method, tc.url, l.oliveTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s, want %d", name, resp.StatusCode, b, tc.want)
		}
	}

	if resp, _ := l.do(t, "DELETE", colURL(col.ID, ""), l.oliveTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := l.do(t, "GET", colURL(col.ID, ""), l.oliveTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404", resp.StatusCode)
	}
}

// Another user's collection is 404 on every route (never 403: its existence is
// not confirmed), never listed, and left unchanged.
func TestCollectionsStrangerIs404(t *testing.T) {
	l := newListsEnv(t)
	col := l.createCollection(t, l.oliveTok, "Olive's")
	l.do(t, "POST", colURL(col.ID, "/items"), l.oliveTok, l.addJSON(cradleBook, -1))

	for _, tc := range []struct{ method, url, body string }{
		{"GET", colURL(col.ID, ""), ""},
		{"PATCH", colURL(col.ID, ""), `{"name":"Mine"}`},
		{"PUT", colURL(col.ID, "/items"), l.itemsJSON(mistbornBook)},
		{"POST", colURL(col.ID, "/items"), l.addJSON(mistbornBook, -1)},
		{"DELETE", colURL(col.ID, "/items") + l.removeQuery(cradleBook), ""},
		{"PUT", colURL(col.ID, "/shares"), fmt.Sprintf(`{"user_ids":[%d]}`, l.sam)},
		{"DELETE", colURL(col.ID, ""), ""},
	} {
		if resp, b := l.do(t, tc.method, tc.url, l.samTok, tc.body); resp.StatusCode != http.StatusNotFound {
			t.Errorf("stranger %s %s = %d %s, want 404", tc.method, tc.url, resp.StatusCode, b)
		}
	}
	// Allowed: the owner still has it, unchanged; denied: the stranger lists nothing.
	got, _ := l.detail(t, l.oliveTok, col.ID)
	if got.Collection.Name != "Olive's" || itemPathsOf(got.Items) != cradleBook || string(got.Collection.SharedWith) != "[]" {
		t.Fatalf("after a stranger's writes = %+v", got)
	}
	if _, body := l.do(t, "GET", "/api/v1/me/collections", l.samTok, ""); body != "{\"collections\":[]}\n" {
		t.Fatalf("stranger's list = %s", body)
	}
}

// A viewer reads a shared collection (listed after their own, without the share
// list), sees only the items THEIR access allows (the owner's out-of-reach item
// and its count never show), gets 403 not_owner on every write, and can leave.
func TestCollectionsViewer(t *testing.T) {
	l := newListsEnv(t)
	col := l.createCollection(t, l.oliveTok, "Family")
	l.do(t, "PUT", colURL(col.ID, "/items"), l.oliveTok, l.itemsJSON(mistbornBook, cradleBook, thread))
	resp, body := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, fmt.Sprintf(`{"user_ids":[%d]}`, l.kid))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"shared_with":[{"id":`+strconv.FormatInt(l.kid, 10)+`,"username":"kid"}]`) {
		t.Fatalf("share = %d %s", resp.StatusCode, body)
	}
	kidsOwn := l.createCollection(t, l.kidTok, "Kid's own")
	// Kid's own collection takes only what kid can reach: an out-of-scope add is
	// 403, a replace skips it.
	if resp, _ := l.do(t, "POST", colURL(kidsOwn.ID, "/items"), l.kidTok, l.addJSON(mistbornBook, -1)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope add = %d, want 403", resp.StatusCode)
	}
	if resp, b := l.do(t, "PUT", colURL(kidsOwn.ID, "/items"), l.kidTok, l.itemsJSON(mistbornBook, cradleBook)); resp.StatusCode != http.StatusOK ||
		strings.Contains(b, "Mistborn") || !strings.Contains(b, cradleBook) {
		t.Fatalf("replace with an out-of-scope book = %d %s", resp.StatusCode, b)
	}

	// The owner sees everything.
	got, _ := l.detail(t, l.oliveTok, col.ID)
	if got.Collection.ItemCount != 3 || len(got.Items) != 3 {
		t.Fatalf("owner's view = %+v", got)
	}
	// The viewer: Mistborn (outside kid's share) is invisible, count and preview included.
	got, body = l.detail(t, l.kidTok, col.ID)
	if got.Collection.Owned || got.Collection.SharedWith != nil || got.Collection.ItemCount != 2 ||
		itemPathsOf(got.Items) != cradleBook+"|"+thread || len(got.Collection.Preview) != 2 || strings.Contains(body, "Mistborn") {
		t.Fatalf("viewer's view = %s", body)
	}
	_, body = l.do(t, "GET", "/api/v1/me/collections", l.kidTok, "")
	var list struct {
		Collections []wireCollection `json:"collections"`
	}
	json.Unmarshal([]byte(body), &list)
	if len(list.Collections) != 2 || list.Collections[0].ID != kidsOwn.ID || list.Collections[0].ItemCount != 1 || list.Collections[1].ID != col.ID ||
		list.Collections[1].ItemCount != 2 || list.Collections[1].SharedWith != nil || strings.Contains(body, "Mistborn") {
		t.Fatalf("viewer's list = %s", body)
	}

	for _, tc := range []struct{ method, url, body string }{
		{"PATCH", colURL(col.ID, ""), `{"name":"Mine"}`},
		{"PUT", colURL(col.ID, "/items"), l.itemsJSON(cradleBook)},
		{"POST", colURL(col.ID, "/items"), l.addJSON(cradleBook, 0)},
		{"DELETE", colURL(col.ID, "/items") + l.removeQuery(cradleBook), ""},
		{"PUT", colURL(col.ID, "/shares"), `{"user_ids":[]}`},
	} {
		resp, b := l.do(t, tc.method, tc.url, l.kidTok, tc.body)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(b, `"code":"not_owner"`) {
			t.Errorf("viewer %s %s = %d %s, want 403 not_owner", tc.method, tc.url, resp.StatusCode, b)
		}
	}
	got, _ = l.detail(t, l.oliveTok, col.ID)
	if got.Collection.Name != "Family" || got.Collection.ItemCount != 3 {
		t.Fatalf("after the viewer's writes = %+v", got)
	}

	// The viewer leaves.
	if resp, _ := l.do(t, "DELETE", colURL(col.ID, ""), l.kidTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("leave = %d", resp.StatusCode)
	}
	if resp, _ := l.do(t, "GET", colURL(col.ID, ""), l.kidTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after leaving = %d, want 404", resp.StatusCode)
	}
	got, _ = l.detail(t, l.oliveTok, col.ID)
	if string(got.Collection.SharedWith) != "[]" || got.Collection.ItemCount != 3 {
		t.Fatalf("owner's after the viewer left = %+v", got)
	}
}

// Shares name only existing, enabled, non-demo users other than the owner (else
// 400, nothing changed); share targets list exactly those; a demo account may do
// neither.
func TestCollectionSharesAndTargets(t *testing.T) {
	l := newListsEnv(t)
	col := l.createCollection(t, l.oliveTok, "Mine")
	for name, id := range map[string]int64{"unknown": 9999, "disabled": l.dora, "demo": l.demo, "self": l.olive} {
		resp, b := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, fmt.Sprintf(`{"user_ids":[%d,%d]}`, l.sam, id))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "unknown user") {
			t.Errorf("share with %s = %d %s, want 400 unknown user", name, resp.StatusCode, b)
		}
	}
	got, _ := l.detail(t, l.oliveTok, col.ID)
	if string(got.Collection.SharedWith) != "[]" {
		t.Fatalf("a rejected share changed it: %s", got.Collection.SharedWith)
	}
	ids := make([]string, catalog.MaxCollectionShares+1)
	for i := range ids {
		ids[i] = strconv.Itoa(1000 + i)
	}
	if resp, _ := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, `{"user_ids":[`+strings.Join(ids, ",")+`]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("share with %d = %d, want 400", len(ids), resp.StatusCode)
	}

	resp, body := l.do(t, "GET", "/api/v1/me/share-targets", l.oliveTok, "")
	want := fmt.Sprintf(`{"users":[{"id":%d,"username":"admin"},{"id":%d,"username":"kid"},{"id":%d,"username":"sam"}]}`+"\n",
		l.adminID, l.kid, l.sam)
	if resp.StatusCode != http.StatusOK || body != want {
		t.Fatalf("share targets = %d %s, want %s", resp.StatusCode, body, want)
	}

	// A demo account: no share targets, no sharing (even its own collection).
	if resp, _ := l.do(t, "GET", "/api/v1/me/share-targets", l.demoTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo share targets = %d, want 403", resp.StatusCode)
	}
	demoCol := l.createCollection(t, l.demoTok, "Demo's")
	if resp, _ := l.do(t, "PUT", colURL(demoCol.ID, "/shares"), l.demoTok, fmt.Sprintf(`{"user_ids":[%d]}`, l.olive)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo share = %d, want 403", resp.StatusCode)
	}
}

// The caps over HTTP: 100 collections (409 collections_full), name and
// description lengths, 1000 items (a replace of more is 400; an add to a full
// one 409 collection_full).
func TestCollectionLimitsHTTP(t *testing.T) {
	l := newListsEnv(t)
	ctx := context.Background()
	if resp, _ := l.do(t, "POST", "/api/v1/me/collections", l.oliveTok,
		fmt.Sprintf(`{"name":%q,"description":%q}`, strings.Repeat("n", 100), strings.Repeat("d", 1000))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create at the length limits = %d", resp.StatusCode)
	}
	if resp, _ := l.do(t, "POST", "/api/v1/me/collections", l.oliveTok,
		fmt.Sprintf(`{"name":"x","description":%q}`, strings.Repeat("d", 1001))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with a long description = %d, want 400", resp.StatusCode)
	}
	if resp, _ := l.do(t, "POST", "/api/v1/me/collections", l.oliveTok,
		fmt.Sprintf(`{"name":%q}`, strings.Repeat("n", 101))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with a long name = %d, want 400", resp.StatusCode)
	}
	var first wireCollection
	for i := 1; i < catalog.MaxCollections; i++ {
		col := l.createCollection(t, l.oliveTok, fmt.Sprintf("C%d", i))
		if i == 1 {
			first = col
		}
	}
	resp, b := l.do(t, "POST", "/api/v1/me/collections", l.oliveTok, `{"name":"one more"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(b, `"code":"collections_full"`) {
		t.Fatalf("create past the cap = %d %s", resp.StatusCode, b)
	}

	many := make([]string, catalog.MaxCollectionItems+1)
	for i := range many {
		many[i] = fmt.Sprintf("Will Wight/x%04d", i)
	}
	if resp, b := l.do(t, "PUT", colURL(first.ID, "/items"), l.oliveTok, l.itemsJSON(many...)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replace with %d = %d %s", len(many), resp.StatusCode, b)
	}
	if resp, b := l.do(t, "PUT", colURL(first.ID, "/items"), l.oliveTok, l.itemsJSON(many[:catalog.MaxCollectionItems]...)); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace with %d = %d %s", catalog.MaxCollectionItems, resp.StatusCode, b)
	}
	for i := range catalog.MaxCollectionItems {
		if err := l.cat.AddCollectionItem(ctx, first.ID, l.olive, catalog.Ref{LibraryID: l.libID, Path: many[i]}, nil); err != nil {
			t.Fatal(err)
		}
	}
	resp, b = l.do(t, "POST", colURL(first.ID, "/items"), l.oliveTok, l.addJSON(cradleBook, -1))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(b, `"code":"collection_full"`) {
		t.Fatalf("add to a full collection = %d %s", resp.StatusCode, b)
	}
}
