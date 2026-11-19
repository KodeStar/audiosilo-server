package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// endlessList is a JSON array of empty objects that never ends: "[{},{},...".
// It counts the bytes read from it.
type endlessList struct{ read int }

func (l *endlessList) Read(p []byte) (int, error) {
	for i := range p {
		if l.read == 0 {
			p[i] = '['
		} else {
			p[i] = "{},"[(l.read-1)%3]
		}
		l.read++
	}
	return len(p), nil
}

// A list past maxBulkBooks is refused at the first entry past it: decoding an
// endless list ends, having read little more than maxBulkBooks entries, so the
// rest of a long one is never decoded (a body bounded for 1000 long paths
// holds over a million empty objects).
func TestDecodeBoundedStopsAtTheCap(t *testing.T) {
	t.Parallel()
	src := &endlessList{}
	list, err := decodeBounded[catalog.Ref](src, maxBulkBooks)
	if !errors.Is(err, errTooManyEntries) || list != nil {
		t.Fatalf("an endless list = %d entries, %v; want errTooManyEntries", len(list), err)
	}
	// The decoder reads ahead in chunks, so allow a few of them past the
	// 3 bytes each entry takes.
	if limit := 3*(maxBulkBooks+1) + 64<<10; src.read > limit {
		t.Fatalf("read %d bytes of an endless list, want at most %d", src.read, limit)
	}

	atCap := "[" + strings.TrimSuffix(strings.Repeat(`{"library_id":1,"path":"p"},`, maxBulkBooks), ",") + "]"
	for name, tc := range map[string]struct {
		body string
		n    int
		ok   bool
		want error // the error itself, when it is one decodeBody tells apart
	}{
		"at the cap":    {atCap, maxBulkBooks, true, nil},
		"past the cap":  {"[{}," + atCap[1:], 0, false, errTooManyEntries},
		"null":          {"null", 0, true, nil},
		"empty":         {"[]", 0, true, nil},
		"unknown field": {`[{"library_id":1,"path":"p","extra":1}]`, 0, false, nil},
		"not a list":    {`{"library_id":1}`, 0, false, nil},
		"not an object": {`[1]`, 0, false, nil},
		"unclosed":      {`[{}`, 0, false, nil},
	} {
		var req struct {
			Books boundedList[catalog.Ref] `json:"books"`
		}
		err := json.Unmarshal([]byte(`{"books":`+tc.body+`}`), &req)
		if (err == nil) != tc.ok || len(req.Books) != tc.n || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: %d entries, %v", name, len(req.Books), err)
		}
	}

	// null is no list and [] an empty one, as a plain slice reads them, also
	// behind a pointer (a whole-list replace tells "name no list" from "empty it").
	var plain struct {
		Null, Empty       boundedList[catalog.Ref]
		NullPtr, EmptyPtr *boundedList[catalog.Ref]
	}
	if err := json.Unmarshal([]byte(`{"Null":null,"Empty":[],"NullPtr":null,"EmptyPtr":[]}`), &plain); err != nil ||
		plain.Null != nil || plain.Empty == nil || plain.NullPtr != nil || plain.EmptyPtr == nil || len(*plain.EmptyPtr) != 0 {
		t.Errorf("null and []: %+v, %v", plain, err)
	}
}

// Every route that takes a list of paths refuses a huge list of empty objects
// (one that fits the body's size bound) with 400 too_large, not a plain 400,
// and makes nothing.
func TestSelectionRoutesRefuseHugeLists(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, _, _ := adminAndMember(t, e)
	share, err := e.cat.CreateShare(context.Background(), catalog.Share{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	// About 1.3 million entries, inside selectionBodyMax; the works route's
	// body is decodeJSON's 1 MiB, and a list of empty objects that fits it is
	// past maxBulkBooks too.
	huge := "[" + strings.Repeat("{},", (selectionBodyMax-1024)/3) + "{}]"
	works := "[" + strings.Repeat("{},", 300_000) + "{}]"
	// A bulk edit's own lists are held to the 1 MiB an edit has, not the room
	// its books have: one that would fill the body is refused before it is read.
	labels := "[" + strings.Repeat(`"",`, (bulkEditBodyMax-1024)/3) + `""]`
	oneBook := `"books":[{"library_id":1,"path":"p"}]`
	for _, tc := range []struct{ method, route, body string }{
		{"POST", "/api/v1/admin/shares", `{"name":"Huge","paths":` + huge + `}`},
		{"POST", "/api/v1/admin/shares/" + strconv.FormatInt(share.ID, 10) + "/paths", `{"rules":` + huge + `}`},
		{"POST", "/api/v1/admin/books/bulk", `{"set":{"narrator":"x"},"books":` + huge + `}`},
		{"POST", "/api/v1/admin/books/bulk", `{` + oneBook + `,"revert":` + labels + `}`},
		{"POST", "/api/v1/admin/books/bulk", `{"add":{"tags":` + labels + `},` + oneBook + `}`},
		{"POST", "/api/v1/admin/books/bulk", `{"chapters":{"revert":` + labels + `},` + oneBook + `,"books":[]}`},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + huge + `}`},
		{"DELETE", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + huge + `}`},
		{"POST", "/api/v1/admin/books/works", `{"books":` + works + `}`},
		{"POST", "/api/v1/admin/covers", `{"books":` + works + `}`},
		{"POST", "/api/v1/admin/meta/covers", `{"urls":` + labels[:1+3*300_000] + `""]}`},
	} {
		resp, out := e.do(t, tc.method, tc.route, adminTok, tc.body)
		wantCode(t, tc.method+" "+tc.route+" with a huge list", resp, out, http.StatusBadRequest, codeTooLarge)
	}
	var made int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares WHERE name = 'Huge'`).Scan(&made); err != nil || made != 0 {
		t.Fatalf("shares made by the refused create: %d, %v", made, err)
	}
	if rules, err := e.cat.ListSharePaths(context.Background(), share.ID); err != nil || len(rules) != 0 {
		t.Fatalf("rules added by the refused add: %v, %v", rules, err)
	}
}

// A selection of as many books as one request takes, each path at
// catalog.MaxPathBytes, gets the route's own answer: its body is past the
// 1 MiB other admin bodies take, so these routes read more (a bulk edit, a
// Health ignore and its undo; the works route takes maxWorkBooks, which fit
// 1 MiB). A body past even that is 400 too_large, never a plain 400.
func TestSelectionRoutesTakeLongPaths(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	refs := func(n int) string {
		list := make([]string, n)
		for i := range list {
			p := fmt.Sprintf("%04d/", i)
			list[i] = fmt.Sprintf(`{"library_id":%d,"path":"%s"}`, libID, p+strings.Repeat("a", catalog.MaxPathBytes-len(p)))
		}
		return "[" + strings.Join(list, ",") + "]"
	}
	full := refs(maxBulkBooks)
	if len(full) <= 1<<20 {
		t.Fatalf("precondition: %d long paths take %d bytes, want past 1 MiB", maxBulkBooks, len(full))
	}
	for _, tc := range []struct {
		method, route, body string
		want                int
	}{
		// No book is indexed at these paths: the bulk edit's own answer.
		{"POST", "/api/v1/admin/books/bulk", `{"set":{"narrator":"x"},"books":` + full + `}`, http.StatusNotFound},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + full + `}`, http.StatusNoContent},
		{"DELETE", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + full + `}`, http.StatusNoContent},
		{"POST", "/api/v1/admin/books/works", `{"books":` + refs(maxWorkBooks) + `}`, http.StatusOK},
	} {
		if resp, out := e.do(t, tc.method, tc.route, adminTok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s %s with %d bytes of long paths = %d %.200s, want %d", tc.method, tc.route, len(tc.body), resp.StatusCode, out, tc.want)
		}
	}
	pad := strings.Repeat("a", bulkEditBodyMax)
	for _, tc := range []struct{ method, route, body string }{
		{"POST", "/api/v1/admin/books/bulk", `{"set":{"narrator":"` + pad + `"},"books":[]}`},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"` + pad + `","books":[]}`},
		{"POST", "/api/v1/admin/books/works", `{"books":[{"library_id":1,"path":"` + pad[:1<<20] + `"}]}`},
	} {
		resp, out := e.do(t, tc.method, tc.route, adminTok, tc.body)
		wantCode(t, tc.method+" "+tc.route+" with a body past its bound", resp, out, http.StatusBadRequest, codeTooLarge)
	}
}

// A route whose list has a cap of its own, under maxBulkBooks, gives that
// cap's answer however far past it the list is: just past it (counted after
// the decode) and past maxBulkBooks (where the decode stops reading).
func TestListRoutesKeepTheirOwnCap(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, _, _ := adminAndMember(t, e)
	list := func(entry string, n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(entry+",", n), ",") + "]"
	}
	ref := `{"library_id":1,"path":"p"}`
	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		method, route, body string // body takes the list at %s
		entry               string
		max                 int
		want                string
	}{
		{"POST", "/api/v1/admin/books/works", `{"books":%s}`, ref, maxWorkBooks,
			`"code":"too_large","error":"too many books in one request (at most 100)"`},
		{"POST", "/api/v1/admin/covers", `{"books":%s}`, ref, maxCoverBatch,
			`"code":"too_large","error":"too many covers in one request (at most 60)"`},
		{"POST", "/api/v1/admin/meta/covers", `{"urls":%s}`, `"https://example.com/c.jpg"`, maxCommunityCovers,
			`"code":"too_large","error":"too many covers in one request (at most 12)"`},
		{"PUT", "/api/v1/me/queue", `{"items":%s}`, ref, catalog.MaxQueue, `"error":"too many items"`},
		{"POST", "/api/v1/me/listening/sync", `{"device_id":"d","sent_at":"` + now + `","events":%s}`, `{}`, catalog.MaxSyncEvents,
			`"code":"too_many_events"`},
	} {
		for _, n := range []int{tc.max + 1, maxBulkBooks + 1} {
			resp, out := e.do(t, tc.method, tc.route, adminTok, fmt.Sprintf(tc.body, list(tc.entry, n)))
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, tc.want) {
				t.Errorf("%s %s with %d entries = %d %.200s, want 400 %s", tc.method, tc.route, n, resp.StatusCode, out, tc.want)
			}
		}
	}
}

// valuesDecoded counts the decodedValues decoded (TestDecodeBoundedMapCountsFirst
// alone uses them).
var valuesDecoded int

type decodedValue struct{}

func (*decodedValue) UnmarshalJSON([]byte) error {
	valuesDecoded++
	return nil
}

// A map past its cap is refused having decoded none of its values: its
// entries are counted first, each skipped, and only a map at the cap becomes
// values.
func TestDecodeBoundedMapCountsFirst(t *testing.T) {
	t.Parallel()
	entries := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf(`"%d":{"a":[1,2]}`, i)
		}
		return "{" + strings.Join(list, ",") + "}"
	}
	if m, err := decodeBoundedMap[int, json.RawMessage]([]byte(entries(5)), 5); err != nil || len(m) != 5 || string(m[4]) != `{"a":[1,2]}` {
		t.Fatalf("at the cap: %v, %v", m, err)
	}
	if m, err := decodeBoundedMap[string, decodedValue]([]byte(entries(5)), 5); err != nil || len(m) != 5 || valuesDecoded != 5 {
		t.Fatalf("at the cap: %d entries, %d values decoded, %v", len(m), valuesDecoded, err)
	}
	valuesDecoded = 0
	if m, err := decodeBoundedMap[string, decodedValue]([]byte(entries(100_000)), 5); !errors.Is(err, errTooManyEntries) || m != nil || valuesDecoded != 0 {
		t.Fatalf("past the cap: %d entries, %d values decoded, %v; want errTooManyEntries and none", len(m), valuesDecoded, err)
	}
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"null": {"null", true}, "empty": {"{}", true}, "not a map": {"[]", false}, "bad value": {`{"1":"x"}`, false},
	} {
		if m, err := decodeBoundedMap[int, int]([]byte(tc.body), 5); (err == nil) != tc.ok || len(m) != 0 {
			t.Errorf("%s: %v, %v", name, m, err)
		}
	}
}

// The lists and maps a one-book edit, a chapter edit and the Bridge's Audible
// titles call carry stop at their caps as they are read: each body holds one
// entry past the cap, and that entry doesn't decode, so the route gives its
// cap's answer only when the decode stopped before it (decoded whole, the
// bad entry would make it a plain 400). One entry fewer is not refused for
// its length.
func TestEditAndTitleListsStopAtTheirCaps(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	lib := strconv.FormatInt(b.lib.ID, 10)
	edit := "/api/v1/admin/libraries/" + lib + "/book?path=" + escape("Some/Book")
	chapters := "/api/v1/admin/libraries/" + lib + "/book/chapters?path=" + escape("Some/Book")
	titles := "/api/v1/bridge/audible/accounts/" + testAccount + "/titles"
	// list is n entries of entry(i), then the bad one when bad isn't "".
	list := func(open, close string, n int, entry func(int) string, bad string) string {
		parts := make([]string, n, n+1)
		for i := range parts {
			parts[i] = entry(i)
		}
		if bad != "" {
			parts = append(parts, bad)
		}
		return open + strings.Join(parts, ",") + close
	}
	arr := func(n int, entry string, bad string) string {
		return list("[", "]", n, func(int) string { return entry }, bad)
	}
	keyed := func(n int, value string, bad string) string {
		return list("{", "}", n, func(i int) string { return fmt.Sprintf(`"%d":%s`, i, value) }, bad)
	}
	basis := `{"files":[{"path":"a.mp3","size":1,"duration_ms":1000}]}`
	tooLongEdit := `"code":"too_large","error":"a list in the edit holds too many entries"`
	for _, tc := range []struct {
		name, method, route, tok string
		body                     func(n int, bad string) string
		max                      int
		bad, want                string
	}{
		{"set", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"set":` + keyed(n, `""`, bad) + `}` },
			maxEditFields, `"x":1`, tooLongEdit},
		{"revert", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"revert":` + arr(n, `"x"`, bad) + `}` },
			maxEditFields, `1`, tooLongEdit},
		{"add fields", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"add":` + keyed(n, `null`, bad) + `}` },
			maxEditFields, `"x":1`, tooLongEdit},
		{"remove labels", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"remove":{"tags":` + arr(n, `"x"`, bad) + `}}` },
			maxEditLabels, `1`, tooLongEdit},
		{"chapter titles", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"chapters":{"set":` + keyed(n, `"x"`, bad) + `}}` },
			maxEditChapters, `"x":"y"`, tooLongEdit},
		{"chapter reverts", "PATCH", edit, b.adminTok, func(n int, bad string) string { return `{"chapters":{"revert":` + arr(n, `1`, bad) + `}}` },
			maxEditChapters, `"x"`, tooLongEdit},
		{"bulk edit labels", "POST", "/api/v1/admin/books/bulk", b.adminTok, func(n int, bad string) string {
			return `{"books":[{"library_id":1,"path":"p"}],"add":{"tags":` + arr(n, `"x"`, bad) + `}}`
		}, maxEditLabels, `1`, tooLongEdit},
		{"edited chapters", "PUT", chapters, b.adminTok, func(n int, bad string) string {
			return `{"basis":` + basis + `,"chapters":` + arr(n, `{"start":1,"title":"x"}`, bad) + `}`
		}, metadata.MaxChapterListEntries, `{"start":"x"}`, `"code":"chapters_invalid","detail":"too_many"`},
		{"basis files", "PUT", chapters, b.adminTok, func(n int, bad string) string {
			return `{"basis":{"files":` + arr(n, `{"path":"a.mp3","size":1,"duration_ms":1000}`, bad) + `},"chapters":[]}`
		}, maxEditChapters, `{"path":1}`, `"code":"chapters_invalid","detail":"too_many"`},
		{"audible titles", "PUT", titles, b.bridgeTok, func(n int, bad string) string {
			return `{"poll_id":"p1","final":false,"titles":` + arr(n, `{}`, bad) + `}`
		}, catalog.MaxAudibleTitles, `{"asin":1}`, `"code":"invalid_request","error":"invalid audible request: at most 500 titles per call"`},
	} {
		resp, out := b.do(t, tc.method, tc.route, tc.tok, tc.body(tc.max, tc.bad))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, tc.want) {
			t.Errorf("%s past its cap of %d: %d %.300s, want 400 %s", tc.name, tc.max, resp.StatusCode, out, tc.want)
		}
		// At the cap the list is read whole, so the bad entry is reached.
		resp, out = b.do(t, tc.method, tc.route, tc.tok, tc.body(tc.max-1, tc.bad))
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(out, tc.want) {
			t.Errorf("%s at its cap of %d: %d %.300s, want a plain 400", tc.name, tc.max, resp.StatusCode, out)
		}
		// A list at the cap is not refused for its length.
		if resp, out = b.do(t, tc.method, tc.route, tc.tok, tc.body(tc.max, "")); strings.Contains(out, tc.want) {
			t.Errorf("%s at its cap of %d: %d %.300s, refused for its length", tc.name, tc.max, resp.StatusCode, out)
		}
	}
}
