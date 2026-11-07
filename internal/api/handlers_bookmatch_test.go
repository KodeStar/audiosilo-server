package api

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestAdminEditBookCommunityMatch (server#320): the match dialog's accept sends
// the community match with its values, and a book matched with no ASIN or ISBN
// (a store listing) is matched: the book page carries community_match (work,
// recording, how "accepted", when, the work's page) and the list says matched.
// {"work_id":""} unlinks it: the community values go with the record, so the book
// is Not matched and shows its own title again. Denied: a member and an anonymous
// caller, the match with an admin's own edit (source edited), a work id with a
// control character, and a bulk edit naming one; none of them records a match. A
// member's unlink and a bulk unlink are refused too, leaving the match whole.
func TestAdminEditBookCommunityMatch(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	url := base + "/book?path=" + escape("Andy Weir/Artemis")
	type detail struct {
		Book struct {
			Matched bool   `json:"matched"`
			Title   string `json:"title"`
		} `json:"book"`
		CommunityMatch *struct {
			WorkID      string `json:"work_id"`
			RecordingID string `json:"recording_id"`
			How         string `json:"how"`
			MatchedAt   string `json:"matched_at"`
			WebURL      string `json:"web_url"`
		} `json:"community_match"`
	}
	read := func(body string) detail {
		t.Helper()
		var d detail
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	matched := func() bool {
		t.Helper()
		_, body := e.do(t, "GET", url, adminTok, "")
		d := read(body)
		return d.CommunityMatch != nil || d.Book.Matched
	}

	// Denied first: nothing is recorded.
	for name, tc := range map[string]struct {
		tok, body string
		want      int
	}{
		"member":            {memberTok, `{"set":{"title":"Artemis"},"source":"community","match":{"work_id":"artemis"}}`, 403},
		"anonymous":         {"", `{"set":{"title":"Artemis"},"source":"community","match":{"work_id":"artemis"}}`, 401},
		"an admin's edit":   {adminTok, `{"set":{"title":"Artemis"},"match":{"work_id":"artemis"}}`, 400},
		"a control char":    {adminTok, `{"source":"community","match":{"work_id":"art\nemis"}}`, 400},
		"unknown match key": {adminTok, `{"source":"community","match":{"work_id":"artemis","how":"auto"}}`, 400},
	} {
		if resp, body := e.do(t, "PATCH", url, tc.tok, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, tc.want)
		}
	}
	lib := strconv.FormatInt(libID, 10)
	bulk := `{"books":[{"library_id":` + lib + `,"path":"Andy Weir/Artemis"}],"source":"community","match":{"work_id":"artemis"}}`
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, bulk); resp.StatusCode != 400 {
		t.Errorf("bulk with a match = %d %s, want 400", resp.StatusCode, body)
	}
	if matched() {
		t.Fatal("a refused request recorded a match")
	}

	// Allowed: the accept with no identifier.
	resp, body := e.do(t, "PATCH", url, adminTok,
		`{"set":{"title":"Artemis: A Novel"},"source":"community","match":{"work_id":"artemis","recording_id":"rec-1"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("accept = %d %s", resp.StatusCode, body)
	}
	d := read(body)
	if m := d.CommunityMatch; m == nil || m.WorkID != "artemis" || m.RecordingID != "rec-1" || m.How != "accepted" ||
		m.MatchedAt == "" || !strings.HasSuffix(m.WebURL, "/work?id=artemis") || !d.Book.Matched || d.Book.Title != "Artemis: A Novel" {
		t.Fatalf("accept response = %s", body)
	}
	_, list := e.do(t, "GET", "/api/v1/admin/books?library_id="+lib+"&matched=true", adminTok, "")
	if !strings.Contains(list, `"path":"Andy Weir/Artemis"`) || strings.Contains(list, `"path":"Andy Weir/The Martian"`) {
		t.Fatalf("matched=true = %s", list)
	}
	// The audit names the work.
	_, audit := e.do(t, "GET", "/api/v1/admin/audit?action=book.edit", adminTok, "")
	if !strings.Contains(audit, `"match":"artemis"`) {
		t.Errorf("audit = %s, want the match named", audit)
	}

	// The unlink: refused for a member and in a bulk edit, the match left whole.
	if resp, body := e.do(t, "PATCH", url, memberTok, `{"match":{"work_id":""}}`); resp.StatusCode != 403 {
		t.Errorf("a member's unlink = %d %s, want 403", resp.StatusCode, body)
	}
	unlinkBulk := `{"books":[{"library_id":` + lib + `,"path":"Andy Weir/Artemis"}],"match":{"work_id":""}}`
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok, unlinkBulk); resp.StatusCode != 400 {
		t.Errorf("a bulk unlink = %d %s, want 400", resp.StatusCode, body)
	}
	if _, body := e.do(t, "GET", url, adminTok, ""); read(body).CommunityMatch == nil || read(body).Book.Title != "Artemis: A Novel" {
		t.Fatalf("a refused unlink changed the book: %s", body)
	}
	// Allowed: the anchor alone takes the community values with it.
	resp, body = e.do(t, "PATCH", url, adminTok, `{"match":{"work_id":""}}`)
	if d := read(body); resp.StatusCode != 200 || d.CommunityMatch != nil || d.Book.Matched || d.Book.Title != "Artemis" {
		t.Fatalf("unlink = %d %s, want Not matched with the book's own title", resp.StatusCode, body)
	}
	_, list = e.do(t, "GET", "/api/v1/admin/books?library_id="+lib+"&matched=false", adminTok, "")
	if !strings.Contains(list, `"path":"Andy Weir/Artemis"`) {
		t.Fatalf("matched=false after the unlink = %s", list)
	}
	// A book with none has no community_match key at all (older consoles ignore it).
	_, body = e.do(t, "GET", base+"/book?path="+escape("Andy Weir/The Martian"), adminTok, "")
	if strings.Contains(body, "community_match") {
		t.Fatalf("an unmatched book's page = %s", body)
	}
}
