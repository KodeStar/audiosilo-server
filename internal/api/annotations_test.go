package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Annotations (Phase 4): bookmark labels, the owner's edits by id, and the
// caller's bookmarks, notes and history across books. Run on listsEnv: olive and
// sam see the whole library, kid only "Will Wight".

// sendAs sends body to path as tok and decodes the answer as a T, failing unless
// its status is want.
func sendAs[T any](t *testing.T, e *testEnv, method, path, tok, body string, want int) T {
	t.Helper()
	resp, b := e.do(t, method, path, tok, body)
	var out T
	if resp.StatusCode != want {
		t.Fatalf("%s %s %s = %d %s, want %d", method, path, body, resp.StatusCode, b, want)
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("%s %s: %v (%s)", method, path, err, b)
	}
	return out
}

// addBookmark and addNote POST one as tok at path (201).
func (l *listsEnv) addBookmark(t *testing.T, tok, path, body string) catalog.Bookmark {
	t.Helper()
	return sendAs[catalog.Bookmark](t, l.testEnv, "POST", l.at("bookmarks", path), tok, body, http.StatusCreated)
}

func (l *listsEnv) addNote(t *testing.T, tok, path, body string) catalog.Note {
	t.Helper()
	return sendAs[catalog.Note](t, l.testEnv, "POST", l.at("notes", path), tok, body, http.StatusCreated)
}

// wantError fails unless the answer is status with {"error": msg}.
func wantError(t *testing.T, what string, resp *http.Response, body string, status int, msg string) {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != status || out.Error != msg {
		t.Fatalf("%s = %d %s, want %d %q", what, resp.StatusCode, body, status, msg)
	}
}

// POST accepts a label (and still refuses unknown fields), always answers one
// ("" when none), checks its shape and bounds the note; notes bound their body.
func TestBookmarkLabelOnPost(t *testing.T) {
	l := newListsEnv(t)
	labelled := l.addBookmark(t, l.oliveTok, cradleBook, `{"position":42,"note":"good bit","label":"fell_asleep"}`)
	if labelled.Label != "fell_asleep" || labelled.Note != "good bit" || labelled.Position != 42 {
		t.Fatalf("labelled bookmark = %+v", labelled)
	}
	resp, body := l.do(t, "POST", l.at("bookmarks", cradleBook), l.oliveTok, `{"position":1}`)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"label":""`) {
		t.Fatalf("unlabelled bookmark = %d %s, want label \"\"", resp.StatusCode, body)
	}
	_, body = l.do(t, "GET", l.at("bookmarks", cradleBook), l.oliveTok, "")
	if !strings.Contains(body, `"label":"fell_asleep"`) || !strings.Contains(body, `"label":""`) {
		t.Fatalf("GET bookmarks = %s, want both labels", body)
	}

	for _, c := range []struct{ body, msg string }{
		{`{"position":1,"label":"Quote"}`, "invalid label"},
		{`{"position":1,"label":"fell-asleep"}`, "invalid label"},
		{`{"position":1,"label":"` + strings.Repeat("a", 33) + `"}`, "invalid label"},
		{`{"position":1,"note":"` + strings.Repeat("é", catalog.MaxBookmarkNote+1) + `"}`, "note too long"},
		{`{"position":1,"labels":"quote"}`, "invalid request"}, // still strict
	} {
		resp, b := l.do(t, "POST", l.at("bookmarks", cradleBook), l.oliveTok, c.body)
		wantError(t, "POST "+c.body[:min(len(c.body), 60)], resp, b, http.StatusBadRequest, c.msg)
	}
	l.addBookmark(t, l.oliveTok, cradleBook, `{"note":"`+strings.Repeat("é", catalog.MaxBookmarkNote)+`"}`)

	resp, body = l.do(t, "POST", l.at("notes", cradleBook), l.oliveTok,
		`{"body":"`+strings.Repeat("a", catalog.MaxNoteBody+1)+`"}`)
	wantError(t, "POST an over-long note", resp, body, http.StatusBadRequest, "body too long")
	l.addNote(t, l.oliveTok, cradleBook, `{"body":"`+strings.Repeat("a", catalog.MaxNoteBody)+`"}`)
	for _, kind := range []string{"notes", "bookmarks"} {
		for _, in := range []string{`{"position":-1}`, `{"position":"12"}`} {
			resp, body = l.do(t, "POST", l.at(kind, cradleBook), l.oliveTok, in)
			wantError(t, "POST "+kind+" "+in, resp, body, http.StatusBadRequest, "invalid position")
		}
	}
}

// PATCH /bookmarks/{id}: the owner's partial edit answers the whole bookmark;
// another user's id, an unknown one and one whose book left the caller's access
// are 404 and change nothing.
func TestEditBookmarkRoute(t *testing.T) {
	l := newListsEnv(t)
	bm := l.addBookmark(t, l.oliveTok, cradleBook, `{"position":12,"note":"first","label":"quote"}`)
	idURL := func(id int64) string { return "/api/v1/bookmarks/" + strconv.FormatInt(id, 10) }
	edit := func(body string) catalog.Bookmark {
		t.Helper()
		return sendAs[catalog.Bookmark](t, l.testEnv, "PATCH", idURL(bm.ID), l.oliveTok, body, http.StatusOK)
	}

	want := bm
	want.Label = "relisten"
	if got := edit(`{"label":"relisten"}`); got != want {
		t.Fatalf("PATCH label = %+v, want %+v", got, want)
	}
	want.Note = "second"
	if got := edit(`{"note":"second"}`); got != want {
		t.Fatalf("PATCH note = %+v, want %+v", got, want)
	}
	want.Note, want.Label = "", ""
	if got := edit(`{"label":"","note":""}`); got != want {
		t.Fatalf("PATCH clearing both = %+v, want %+v", got, want)
	}
	stored := func() catalog.Bookmark {
		t.Helper()
		bms, err := l.cat.ListBookmarks(t.Context(), l.olive, catalog.Ref{LibraryID: l.libID, Path: cradleBook})
		if err != nil || len(bms) != 1 {
			t.Fatalf("olive's bookmarks = %+v %v", bms, err)
		}
		return bms[0]
	}
	if s := stored(); s != want {
		t.Fatalf("stored %+v, want %+v", s, want)
	}

	// kid's bookmark on a book a since-revoked share reached.
	grant, revoke := l.sandersonShare(t)
	grant()
	kids := l.addBookmark(t, l.kidTok, mistbornBook, `{"position":3,"label":"funny"}`)
	revoke()

	for _, c := range []struct {
		name, tok string
		id        int64
		body      string
		status    int
		msg       string
	}{
		{"another user's id", l.samTok, bm.ID, `{"note":"mine now"}`, http.StatusNotFound, "bookmark not found"},
		{"an unknown id", l.oliveTok, bm.ID + 1000, `{"note":"x"}`, http.StatusNotFound, "bookmark not found"},
		{"a revoked share's", l.kidTok, kids.ID, `{"note":"x"}`, http.StatusNotFound, "bookmark not found"},
		{"an empty body", l.oliveTok, bm.ID, `{}`, http.StatusBadRequest, "nothing to change"},
		{"no body", l.oliveTok, bm.ID, ``, http.StatusBadRequest, "nothing to change"},
		{"nulls only", l.oliveTok, bm.ID, `{"note":null}`, http.StatusBadRequest, "nothing to change"},
		{"a bad label", l.oliveTok, bm.ID, `{"label":"Nope"}`, http.StatusBadRequest, "invalid label"},
		{"a long note", l.oliveTok, bm.ID, `{"note":"` + strings.Repeat("a", catalog.MaxBookmarkNote+1) + `"}`,
			http.StatusBadRequest, "note too long"},
		{"a field it can't change", l.oliveTok, bm.ID, `{"position":5}`, http.StatusBadRequest, "invalid request"},
	} {
		resp, body := l.do(t, "PATCH", idURL(c.id), c.tok, c.body)
		wantError(t, c.name, resp, body, c.status, c.msg)
		if s := stored(); s != want {
			t.Fatalf("%s changed olive's bookmark: %+v", c.name, s)
		}
	}
	resp, body := l.do(t, "PATCH", "/api/v1/bookmarks/abc", l.oliveTok, `{"note":"x"}`)
	wantError(t, "a non-numeric id", resp, body, http.StatusBadRequest, "invalid bookmark id")

	// kid's row is kept, unchanged, and editable again once access returns.
	grant()
	if got := sendAs[catalog.Bookmark](t, l.testEnv, "PATCH", idURL(kids.ID), l.kidTok, `{"note":"back"}`, http.StatusOK); got.Label != "funny" || got.Note != "back" {
		t.Fatalf("PATCH after re-granting = %+v", got)
	}
}

// PATCH /notes/{id}: body and position, partial, updated_at stamped; the same
// denials as bookmarks, and a position that is negative or not a number is 400.
func TestEditNoteRoute(t *testing.T) {
	l := newListsEnv(t)
	n := l.addNote(t, l.oliveTok, cradleBook, `{"body":"a thought"}`)
	idURL := func(id int64) string { return "/api/v1/notes/" + strconv.FormatInt(id, 10) }
	edit := func(body string) catalog.Note {
		t.Helper()
		return sendAs[catalog.Note](t, l.testEnv, "PATCH", idURL(n.ID), l.oliveTok, body, http.StatusOK)
	}

	got := edit(`{"position":93.5}`)
	if got.Position != 93.5 || got.Body != "a thought" || got.ID != n.ID || got.CreatedAt != n.CreatedAt ||
		got.UpdatedAt == "" || got.Path != cradleBook {
		t.Fatalf("PATCH position = %+v (added %+v)", got, n)
	}
	if got = edit(`{"body":"better","position":0}`); got.Body != "better" || got.Position != 0 {
		t.Fatalf("PATCH both = %+v", got)
	}
	stored := func() catalog.Note {
		t.Helper()
		ns, err := l.cat.ListNotes(t.Context(), l.olive, catalog.Ref{LibraryID: l.libID, Path: cradleBook})
		if err != nil || len(ns) != 1 {
			t.Fatalf("olive's notes = %+v %v", ns, err)
		}
		return ns[0]
	}
	if s := stored(); s != got {
		t.Fatalf("stored %+v, answered %+v", s, got)
	}

	grant, revoke := l.sandersonShare(t)
	grant()
	kids := l.addNote(t, l.kidTok, mistbornBook, `{"body":"kid's"}`)
	revoke()

	for _, c := range []struct {
		name, tok string
		id        int64
		body      string
		status    int
		msg       string
	}{
		{"another user's id", l.samTok, n.ID, `{"body":"mine"}`, http.StatusNotFound, "note not found"},
		{"an unknown id", l.oliveTok, n.ID + 1000, `{"body":"x"}`, http.StatusNotFound, "note not found"},
		{"a revoked share's", l.kidTok, kids.ID, `{"body":"x"}`, http.StatusNotFound, "note not found"},
		{"an empty body", l.oliveTok, n.ID, `{}`, http.StatusBadRequest, "nothing to change"},
		{"no body", l.oliveTok, n.ID, ``, http.StatusBadRequest, "nothing to change"},
		{"a negative position", l.oliveTok, n.ID, `{"position":-1}`, http.StatusBadRequest, "invalid position"},
		{"a string position", l.oliveTok, n.ID, `{"position":"12"}`, http.StatusBadRequest, "invalid position"},
		{"a long body", l.oliveTok, n.ID, `{"body":"` + strings.Repeat("a", catalog.MaxNoteBody+1) + `"}`,
			http.StatusBadRequest, "body too long"},
		{"an unknown field", l.oliveTok, n.ID, `{"label":"quote"}`, http.StatusBadRequest, "invalid request"},
	} {
		resp, body := l.do(t, "PATCH", idURL(c.id), c.tok, c.body)
		wantError(t, c.name, resp, body, c.status, c.msg)
		if s := stored(); s != got {
			t.Fatalf("%s changed olive's note: %+v", c.name, s)
		}
	}
	resp, body := l.do(t, "PATCH", "/api/v1/notes/abc", l.oliveTok, `{"body":"x"}`)
	wantError(t, "a non-numeric id", resp, body, http.StatusBadRequest, "invalid note id")
}

// listRow is a row of GET /me/bookmarks, /me/notes or /me/history, with its book.
type listRow struct {
	ID   int64         `json:"id"`
	Path string        `json:"path"`
	Book *catalog.Book `json:"book"`
}

// myPages walks GET route as tok a page of limit at a time and returns every
// row; key is the envelope's list key.
func (l *listsEnv) myPages(t *testing.T, tok, route, key string, limit int) []listRow {
	t.Helper()
	var all []listRow
	cursor := ""
	for range 50 {
		u := fmt.Sprintf("%s?limit=%d", route, limit)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		resp, body := l.do(t, "GET", u, tok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", u, resp.StatusCode, body)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatal(err)
		}
		var rows []listRow
		if err := json.Unmarshal(env[key], &rows); err != nil || rows == nil {
			t.Fatalf("GET %s: %q = %s (%v)", u, key, env[key], err)
		}
		all = append(all, rows...)
		next := ""
		if raw, ok := env["next_cursor"]; ok {
			_ = json.Unmarshal(raw, &next)
		}
		if next == "" {
			return all
		}
		if len(rows) != limit {
			t.Fatalf("GET %s: a page of %d before the end", u, len(rows))
		}
		cursor = next
	}
	t.Fatalf("GET %s never ends", route)
	return nil
}

func rowIDs(rows []listRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// GET /me/bookmarks, /me/notes and /me/history: the caller's rows only, newest
// first, paged without loss or repeats (history's ended_at ties included), rows a
// revoked share reached left out but kept, each row's book when indexed.
func TestMyAnnotationLists(t *testing.T) {
	l := newListsEnv(t)
	grant, revoke := l.sandersonShare(t)
	grant()
	paths := []string{cradleBook, cradlePart, mistbornBook, thread}
	for i := range 9 {
		p := paths[i%len(paths)]
		l.addBookmark(t, l.kidTok, p, fmt.Sprintf(`{"position":%d,"label":"quote"}`, i))
		l.addNote(t, l.kidTok, p, fmt.Sprintf(`{"body":"n%d","position":%d}`, i, i))
		ended := fmt.Sprintf("2026-10-07T10:0%d:00Z", i/3) // three spans per timestamp
		span := fmt.Sprintf(`{"from_pos":0,"to_pos":%d,"started_at":%q,"ended_at":%q}`, i+1, ended, ended)
		if resp, b := l.do(t, "POST", l.at("history", p), l.kidTok, span); resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST history = %d %s", resp.StatusCode, b)
		}
		// sam's rows on the same books, never kid's to see.
		l.addBookmark(t, l.samTok, p, `{"position":1}`)
		l.addNote(t, l.samTok, p, `{"body":"sam"}`)
		if resp, b := l.do(t, "POST", l.at("history", p), l.samTok, span); resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST history = %d %s", resp.StatusCode, b)
		}
	}

	for _, list := range []struct{ route, key string }{
		{"/api/v1/me/bookmarks", "bookmarks"},
		{"/api/v1/me/notes", "notes"},
		{"/api/v1/me/history", "history"},
	} {
		t.Run(list.key, func(t *testing.T) {
			full := l.myPages(t, l.kidTok, list.route, list.key, 500)
			if len(full) != 9 {
				t.Fatalf("kid's %s = %d rows, want 9", list.key, len(full))
			}
			ids := rowIDs(full)
			if !slices.IsSortedFunc(ids, func(a, b int64) int { return int(b - a) }) {
				t.Fatalf("not newest first: %v", ids) // added a row at a time: newest = highest id
			}
			for _, limit := range []int{1, 2, 4, 9, 10} {
				if got := rowIDs(l.myPages(t, l.kidTok, list.route, list.key, limit)); !slices.Equal(got, ids) {
					t.Fatalf("limit %d: %v, want %v", limit, got, ids)
				}
			}
			for _, r := range full {
				indexed := r.Path != cradlePart
				if (r.Book != nil) != indexed || (indexed && r.Book.RelPath != r.Path) {
					t.Fatalf("row on %s: book %+v (indexed %v)", r.Path, r.Book, indexed)
				}
			}
			sams := rowIDs(l.myPages(t, l.samTok, list.route, list.key, 3))
			if len(sams) != 9 || slices.ContainsFunc(sams, func(id int64) bool { return slices.Contains(ids, id) }) {
				t.Fatalf("sam's %s = %v, kid's %v", list.key, sams, ids)
			}

			// Revoked: the Mistborn rows are left out, the rest still pages whole.
			revoke()
			var want []int64
			for _, r := range full {
				if r.Path != mistbornBook {
					want = append(want, r.ID)
				}
			}
			if got := rowIDs(l.myPages(t, l.kidTok, list.route, list.key, 2)); !slices.Equal(got, want) {
				t.Fatalf("after revoking: %v, want %v", got, want)
			}
			grant()
			if got := rowIDs(l.myPages(t, l.kidTok, list.route, list.key, 4)); !slices.Equal(got, ids) {
				t.Fatalf("after re-granting: %v, want %v", got, ids)
			}

			for _, c := range []string{"!!!", "bm8gc2VwYXJhdG9y"} { // "no separator"
				resp, body := l.do(t, "GET", list.route+"?cursor="+url.QueryEscape(c), l.kidTok, "")
				wantError(t, "a malformed cursor", resp, body, http.StatusBadRequest, "invalid cursor")
			}
		})
	}
}

// GET /me/history keeps today's answer without a cursor (now with books, and
// next_cursor only when more follow), and every list is [] when empty.
func TestMyAnnotationListsShape(t *testing.T) {
	l := newListsEnv(t)
	for _, c := range []struct{ route, want string }{
		{"/api/v1/me/bookmarks", `{"bookmarks":[]}`},
		{"/api/v1/me/notes", `{"notes":[]}`},
		{"/api/v1/me/history", `{"history":[]}`},
	} {
		if resp, body := l.do(t, "GET", c.route, l.samTok, ""); resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != c.want {
			t.Fatalf("GET %s empty = %d %s, want %s", c.route, resp.StatusCode, body, c.want)
		}
	}
	// The per-book lists too ([] now, null before: every player coalesces).
	for _, kind := range []string{"bookmarks", "notes", "history"} {
		want := `{"` + kind + `":[]}`
		if resp, body := l.do(t, "GET", l.at(kind, cradleBook), l.samTok, ""); resp.StatusCode != http.StatusOK ||
			strings.TrimSpace(body) != want {
			t.Fatalf("GET %s on a book, empty = %d %s, want %s", kind, resp.StatusCode, body, want)
		}
	}
	span := `{"from_pos":0,"to_pos":30,"started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:00:30Z"}`
	for range 2 {
		if resp, b := l.do(t, "POST", l.at("history", cradleBook), l.samTok, span); resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST history = %d %s", resp.StatusCode, b)
		}
	}
	_, body := l.do(t, "GET", "/api/v1/me/history", l.samTok, "")
	if strings.Contains(body, "next_cursor") || !strings.Contains(body, `"book":{`) || !strings.Contains(body, `"to_pos":30`) {
		t.Fatalf("GET /me/history = %s", body)
	}
	_, body = l.do(t, "GET", "/api/v1/me/history?limit=1", l.samTok, "")
	if !strings.Contains(body, `"next_cursor":"`) {
		t.Fatalf("GET /me/history?limit=1 = %s, want a next_cursor", body)
	}
	// The list's book is the list shape: no description.
	if strings.Contains(body, `"description"`) {
		t.Fatalf("a list book carries a description: %s", body)
	}
}
