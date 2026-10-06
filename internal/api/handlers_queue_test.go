package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The lists tests' books: cradleBook and mistbornBook (scanned from testdata,
// see fixtures_1b_test.go, with cradlePart a part of Cradle) and thread, indexed directly.
const thread = "Will Wight/Threadlight"

// listsEnv is a library (cradleBook, thread, mistbornBook) and the accounts the queue and collection tests
// need: olive (whole library, owns things), kid (the "Will Wight" folder only),
// sam (whole library, a stranger to olive's things), a demo account (whole
// library) and a disabled one.
type listsEnv struct {
	*testEnv
	libID                       int64
	share                       int64 // kid's "Will Wight" share
	olive, kid, sam, demo, dora int64
	oliveTok, kidTok, samTok    string
	demoTok                     string
}

func newListsEnv(t *testing.T) *listsEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	lib := newFixtureLibrary(t, e)
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: thread, IsFolder: true,
		Title: "Threadlight", Author: "Will Wight"}); err != nil {
		t.Fatal(err)
	}
	l := &listsEnv{testEnv: e, libID: lib.ID}
	l.olive, l.oliveTok = newUserWithSession(t, e, "olive", false)
	l.kid, l.kidTok = newUserWithSession(t, e, "kid", false)
	l.sam, l.samTok = newUserWithSession(t, e, "sam", false)
	l.demo, l.demoTok = newUserWithSession(t, e, "demo_1", true)
	l.dora, _ = newUserWithSession(t, e, "dora", false)
	for _, id := range []int64{l.olive, l.sam, l.demo} {
		if err := e.cat.GrantWholeLibrary(ctx, id, lib.ID); err != nil {
			t.Fatal(err)
		}
	}
	l.share = grantWightOnly(t, e, lib.ID, l.kid)
	if err := e.auth.SetDisabled(ctx, l.dora, true); err != nil {
		t.Fatal(err)
	}
	return l
}

// refJSON is a {library_id, path} body fragment.
func (l *listsEnv) refJSON(path string) string {
	b, _ := json.Marshal(catalog.Ref{LibraryID: l.libID, Path: path})
	return string(b)
}

// addJSON is a list add body, with a position when pos >= 0.
func (l *listsEnv) addJSON(path string, pos int) string {
	in := map[string]any{"library_id": l.libID, "path": path}
	if pos >= 0 {
		in["position"] = pos
	}
	b, _ := json.Marshal(in)
	return string(b)
}

// itemsJSON is a whole-list replace body.
func (l *listsEnv) itemsJSON(paths ...string) string {
	refs := make([]string, len(paths))
	for i, p := range paths {
		refs[i] = l.refJSON(p)
	}
	return `{"items":[` + strings.Join(refs, ",") + `]}`
}

// removeQuery is ?library_id=&path= for a list remove.
func (l *listsEnv) removeQuery(path string) string {
	return "?library_id=" + strconv.FormatInt(l.libID, 10) + "&path=" + url.QueryEscape(path)
}

// wireItem is an up-next entry / collection item as the wire carries it.
type wireItem struct {
	LibraryID int64           `json:"library_id"`
	Path      string          `json:"path"`
	AddedAt   string          `json:"added_at"`
	Book      json.RawMessage `json:"book"`
}

func itemPathsOf(items []wireItem) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Path)
	}
	return strings.Join(out, "|")
}

// queueOf GETs a caller's queue, failing on anything but 200.
func (l *listsEnv) queueOf(t *testing.T, tok string) []wireItem {
	t.Helper()
	resp, body := l.do(t, "GET", "/api/v1/me/queue", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Queue []wireItem `json:"queue"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Queue == nil {
		t.Fatalf("queue body %s: %v", body, err)
	}
	return out.Queue
}

// The capabilities advertise the queue and collections.
func TestListCapabilities(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"queue":true`) || !strings.Contains(body, `"collections":true`) {
		t.Fatalf("/server capabilities: %s", body)
	}
}

// Add (end, at a position, a move, an idempotent re-add), read with the book
// attached in its list shape, remove (idempotent), and the request errors.
func TestQueueRoundTrip(t *testing.T) {
	l := newListsEnv(t)
	post := func(body string) (int, string) {
		t.Helper()
		resp, b := l.do(t, "POST", "/api/v1/me/queue", l.oliveTok, body)
		return resp.StatusCode, b
	}
	if got := l.queueOf(t, l.oliveTok); len(got) != 0 {
		t.Fatalf("fresh queue = %+v", got)
	}
	// A part path stores its book's path.
	for _, p := range []string{cradlePart, thread} {
		if status, b := post(l.addJSON(p, -1)); status != http.StatusOK {
			t.Fatalf("add %s = %d %s", p, status, b)
		}
	}
	status, body := post(l.addJSON(mistbornBook, 0))
	if status != http.StatusOK || !strings.Contains(body, `"queue":[`) {
		t.Fatalf("add at 0 = %d %s", status, body)
	}
	got := l.queueOf(t, l.oliveTok)
	if itemPathsOf(got) != mistbornBook+"|"+cradleBook+"|"+thread {
		t.Fatalf("order = %s", itemPathsOf(got))
	}
	var book map[string]any
	if err := json.Unmarshal(got[0].Book, &book); err != nil || book["rel_path"] != mistbornBook || book["title"] == "" {
		t.Fatalf("book not attached: %s", got[0].Book)
	}
	if _, ok := book["description"]; ok || got[0].AddedAt == "" {
		t.Fatalf("entry not in the list shape: %+v %s", got[0], got[0].Book)
	}
	post(l.addJSON(thread, -1))    // already queued: stays
	post(l.addJSON(cradlePart, 9)) // moves to the end
	if got := itemPathsOf(l.queueOf(t, l.oliveTok)); got != mistbornBook+"|"+thread+"|"+cradleBook {
		t.Fatalf("order after re-add and move = %s", got)
	}

	for range 2 {
		if resp, b := l.do(t, "DELETE", "/api/v1/me/queue"+l.removeQuery(thread), l.oliveTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("remove = %d %s", resp.StatusCode, b)
		}
	}
	if got := itemPathsOf(l.queueOf(t, l.oliveTok)); got != mistbornBook+"|"+cradleBook {
		t.Fatalf("order after remove = %s", got)
	}

	for name, tc := range map[string]struct {
		method, query, body string
		want                int
	}{
		"negative position": {"POST", "", fmt.Sprintf(`{"library_id":%d,"path":%q,"position":-1}`, l.libID, thread), 400},
		"fraction position": {"POST", "", fmt.Sprintf(`{"library_id":%d,"path":%q,"position":1.5}`, l.libID, thread), 400},
		"unknown field":     {"POST", "", `{"library_id":1,"path":"x","extra":1}`, 400},
		"no library":        {"POST", "", `{"path":"` + thread + `"}`, 400},
		"no path":           {"POST", "", fmt.Sprintf(`{"library_id":%d}`, l.libID), 400},
		"not a book":        {"POST", "", l.addJSON("Will Wight/Nope.m4b", -1), 404},
		"unknown library":   {"POST", "", `{"library_id":999,"path":"x"}`, 403},
		"bad replace":       {"PUT", "", `{"items":"no"}`, 400},
		"remove no path":    {"DELETE", "?library_id=" + strconv.FormatInt(l.libID, 10), "", 400},
		"remove bad lib":    {"DELETE", "?library_id=x&path=a", "", 400},
	} {
		if resp, b := l.do(t, tc.method, "/api/v1/me/queue"+tc.query, l.oliveTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s, want %d", name, resp.StatusCode, b, tc.want)
		}
	}
	if resp, _ := l.do(t, "GET", "/api/v1/me/queue", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d", resp.StatusCode)
	}
}

// A queue is its owner's alone: another user never sees it, and their remove or
// replace touches only their own.
func TestQueueIsPrivate(t *testing.T) {
	l := newListsEnv(t)
	if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, l.itemsJSON(cradleBook, mistbornBook)); resp.StatusCode != http.StatusOK {
		t.Fatalf("olive's replace = %d %s", resp.StatusCode, b)
	}
	// Allowed: sam has his own (empty) queue.
	if got := l.queueOf(t, l.samTok); len(got) != 0 {
		t.Fatalf("sam sees %+v", got)
	}
	// Denied: sam's writes don't reach olive's queue.
	l.do(t, "DELETE", "/api/v1/me/queue"+l.removeQuery(cradleBook), l.samTok, "")
	l.do(t, "PUT", "/api/v1/me/queue", l.samTok, l.itemsJSON(thread))
	if got := itemPathsOf(l.queueOf(t, l.oliveTok)); got != cradleBook+"|"+mistbornBook {
		t.Fatalf("olive's queue after sam's writes = %s", got)
	}
	if got := itemPathsOf(l.queueOf(t, l.samTok)); got != thread {
		t.Fatalf("sam's queue = %s", got)
	}
}

// The caller's access gates the queue: an out-of-scope add is 403 and never
// stored, a replace skips what is out of scope or not exactly a book, and a
// revoked share hides a queued book without deleting it (re-granting shows it).
func TestQueueScope(t *testing.T) {
	l := newListsEnv(t)
	ctx := context.Background()
	// Denied: Mistborn is outside kid's share.
	if resp, b := l.do(t, "POST", "/api/v1/me/queue", l.kidTok, l.addJSON(mistbornBook, -1)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope add = %d %s, want 403", resp.StatusCode, b)
	}
	// A path that cleans to outside the share is out of scope too.
	if resp, _ := l.do(t, "POST", "/api/v1/me/queue", l.kidTok, l.addJSON("Will Wight/../"+mistbornBook, -1)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dot-dot add = %d, want 403", resp.StatusCode)
	}
	// Allowed, and the replace's skip rule.
	resp, body := l.do(t, "PUT", "/api/v1/me/queue", l.kidTok,
		l.itemsJSON(mistbornBook, cradleBook, cradlePart, "Will Wight", "Will Wight/Nope.m4b", "/"+cradleBook+"/", "Will Wight/../"+mistbornBook))
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "Mistborn") {
		t.Fatalf("replace = %d %s", resp.StatusCode, body)
	}
	if got := itemPathsOf(l.queueOf(t, l.kidTok)); got != cradleBook {
		t.Fatalf("kid's queue = %s, want only %s", got, cradleBook)
	}

	// Revoked: hidden, not deleted.
	if err := l.cat.RevokeShare(ctx, l.kid, l.share); err != nil {
		t.Fatal(err)
	}
	if got := l.queueOf(t, l.kidTok); len(got) != 0 {
		t.Fatalf("revoked queue still shows %+v", got)
	}
	if err := l.cat.GrantShare(ctx, l.kid, l.share); err != nil {
		t.Fatal(err)
	}
	if got := itemPathsOf(l.queueOf(t, l.kidTok)); got != cradleBook {
		t.Fatalf("re-granted queue = %s", got)
	}
}

// The queue holds MaxQueue books: a replace of more is 400, an add to a full
// queue 409 queue_full, a move within it still works.
func TestQueueLimits(t *testing.T) {
	l := newListsEnv(t)
	ctx := context.Background()
	many := make([]string, catalog.MaxQueue+1)
	for i := range many {
		many[i] = fmt.Sprintf("Will Wight/x%03d.m4b", i)
	}
	if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, l.itemsJSON(many...)); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(b, "too many items") {
		t.Fatalf("replace with %d = %d %s", len(many), resp.StatusCode, b)
	}
	if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, l.itemsJSON(many[:catalog.MaxQueue]...)); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace with %d = %d %s", catalog.MaxQueue, resp.StatusCode, b)
	}
	if err := l.cat.AddToQueue(ctx, l.olive, catalog.Ref{LibraryID: l.libID, Path: cradleBook}, nil); err != nil {
		t.Fatal(err)
	}
	for i := range catalog.MaxQueue - 1 {
		if err := l.cat.AddToQueue(ctx, l.olive, catalog.Ref{LibraryID: l.libID, Path: many[i]}, nil); err != nil {
			t.Fatal(err)
		}
	}
	resp, b := l.do(t, "POST", "/api/v1/me/queue", l.oliveTok, l.addJSON(mistbornBook, -1))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(b, `"code":"queue_full"`) {
		t.Fatalf("add to a full queue = %d %s", resp.StatusCode, b)
	}
	if resp, b := l.do(t, "POST", "/api/v1/me/queue", l.oliveTok, l.addJSON(cradleBook, 3)); resp.StatusCode != http.StatusOK {
		t.Fatalf("move within a full queue = %d %s", resp.StatusCode, b)
	}
}

// A whole-list replace must name its list: an absent or null items (or
// user_ids) is a 400 that changes nothing, never "empty it" (a client's {} would
// otherwise wipe the list, hidden entries included); an explicit [] clears.
func TestListReplaceNeedsTheList(t *testing.T) {
	l := newListsEnv(t)
	if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, l.itemsJSON(cradleBook, mistbornBook)); resp.StatusCode != http.StatusOK {
		t.Fatalf("queue = %d %s", resp.StatusCode, b)
	}
	col := l.createCollection(t, l.oliveTok, "Mine")
	if resp, b := l.do(t, "PUT", colURL(col.ID, "/items"), l.oliveTok, l.itemsJSON(cradleBook)); resp.StatusCode != http.StatusOK {
		t.Fatalf("items = %d %s", resp.StatusCode, b)
	}
	if resp, b := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, fmt.Sprintf(`{"user_ids":[%d]}`, l.sam)); resp.StatusCode != http.StatusOK {
		t.Fatalf("shares = %d %s", resp.StatusCode, b)
	}
	for _, body := range []string{`{}`, `{"items":null}`, `null`} {
		if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT /me/queue %s = %d %s, want 400", body, resp.StatusCode, b)
		}
		if resp, b := l.do(t, "PUT", colURL(col.ID, "/items"), l.oliveTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT items %s = %d %s, want 400", body, resp.StatusCode, b)
		}
	}
	for _, body := range []string{`{}`, `{"user_ids":null}`, `null`} {
		if resp, b := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT shares %s = %d %s, want 400", body, resp.StatusCode, b)
		}
	}
	if got := itemPathsOf(l.queueOf(t, l.oliveTok)); got != cradleBook+"|"+mistbornBook {
		t.Fatalf("queue after the refused replaces = %s", got)
	}
	got, _ := l.detail(t, l.oliveTok, col.ID)
	if itemPathsOf(got.Items) != cradleBook || !strings.Contains(string(got.Collection.SharedWith), `"username":"sam"`) {
		t.Fatalf("collection after the refused replaces = %+v", got)
	}

	// An explicit empty list clears.
	if resp, b := l.do(t, "PUT", "/api/v1/me/queue", l.oliveTok, `{"items":[]}`); resp.StatusCode != http.StatusOK ||
		strings.TrimSpace(b) != `{"queue":[]}` {
		t.Fatalf(`PUT {"items":[]} = %d %s`, resp.StatusCode, b)
	}
	if resp, b := l.do(t, "PUT", colURL(col.ID, "/shares"), l.oliveTok, `{"user_ids":[]}`); resp.StatusCode != http.StatusOK ||
		!strings.Contains(b, `"shared_with":[]`) {
		t.Fatalf(`PUT {"user_ids":[]} = %d %s`, resp.StatusCode, b)
	}
}
