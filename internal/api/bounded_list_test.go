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

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
	}{
		"at the cap":    {atCap, maxBulkBooks, true},
		"past the cap":  {"[{}," + atCap[1:], 0, false},
		"null":          {"null", 0, true},
		"empty":         {"[]", 0, true},
		"unknown field": {`[{"library_id":1,"path":"p","extra":1}]`, 0, false},
		"not a list":    {`{"library_id":1}`, 0, false},
		"not an object": {`[1]`, 0, false},
		"unclosed":      {`[{}`, 0, false},
	} {
		var req struct {
			Books boundedList[catalog.Ref] `json:"books"`
		}
		err := json.Unmarshal([]byte(`{"books":`+tc.body+`}`), &req)
		if (err == nil) != tc.ok || len(req.Books) != tc.n {
			t.Errorf("%s: %d entries, %v", name, len(req.Books), err)
		}
		if name == "past the cap" && !errors.Is(err, errTooManyEntries) {
			t.Errorf("past the cap: %v, want errTooManyEntries", err)
		}
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
	// About 1.3 million entries, inside selectionBodyMax.
	huge := "[" + strings.Repeat("{},", (selectionBodyMax-1024)/3) + "{}]"
	for _, tc := range []struct{ method, route, body string }{
		{"POST", "/api/v1/admin/shares", `{"name":"Huge","paths":` + huge + `}`},
		{"POST", "/api/v1/admin/shares/" + strconv.FormatInt(share.ID, 10) + "/paths", `{"rules":` + huge + `}`},
		{"POST", "/api/v1/admin/books/bulk", `{"set":{"narrator":"x"},"books":` + huge + `}`},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + huge + `}`},
		{"DELETE", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":` + huge + `}`},
	} {
		if resp, out := e.do(t, tc.method, tc.route, adminTok, tc.body); resp.StatusCode != http.StatusBadRequest ||
			!strings.Contains(out, `"code":"`+codeTooLarge+`"`) {
			t.Errorf("%s %s with a huge list = %d %.200s, want 400 too_large", tc.method, tc.route, resp.StatusCode, out)
		}
	}
	// The works route's body is decodeJSON's 1 MiB: a list of empty objects
	// that fits it is past maxBulkBooks, and refused the same way.
	works := `{"books":[` + strings.Repeat("{},", 300_000) + `{}]}`
	if resp, out := e.do(t, "POST", "/api/v1/admin/books/works", adminTok, works); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(out, `"code":"`+codeTooLarge+`"`) {
		t.Errorf("works with a huge list = %d %.200s, want 400 too_large", resp.StatusCode, out)
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
		if resp, out := e.do(t, tc.method, tc.route, adminTok, tc.body); resp.StatusCode != http.StatusBadRequest ||
			!strings.Contains(out, `"code":"`+codeTooLarge+`"`) {
			t.Errorf("%s %s with a body past its bound = %d %.200s, want 400 too_large", tc.method, tc.route, resp.StatusCode, out)
		}
	}
}
