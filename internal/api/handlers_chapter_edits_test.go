package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// seedTwoFileBook indexes a folder book of two half-hour files, one chapter a
// file, and returns the admin book URL and the player's chapters URL for it.
func seedTwoFileBook(t *testing.T, e *testEnv) (adminURL, chaptersURL string) {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const p = "Author/Book"
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Book",
		Duration: 3600, AddedAt: "2024-01-01T00:00:00Z",
		Files: []catalog.BookFile{{RelPath: p + "/01.mp3", Seq: 0, Duration: 1800, Size: 10},
			{RelPath: p + "/02.mp3", Seq: 1, Duration: 1800, Size: 20}},
		Chapters: []metadata.Chapter{{Index: 0, Title: "01", FilePath: p + "/01.mp3", End: 1800},
			{Index: 1, Title: "02", FileIndex: 1, FilePath: p + "/02.mp3", End: 1800, BookOffset: 1800}},
	}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(lib.ID, 10)
	q := "?path=" + escape(p)
	return "/api/v1/admin/libraries/" + id + "/book/chapters" + q, "/api/v1/libraries/" + id + "/chapters" + q
}

const twoFileBasis = `{"files":[{"path":"01.mp3","size":10,"duration_ms":1800000},{"path":"02.mp3","size":20,"duration_ms":1800000}]}`

// TestAdminChapterEditAPI (server#240): the book page carries the basis; an
// admin's PUT stores the edit (the page says applied, the player's /chapters
// and item say chapters_source "edited" with the right file and book offsets)
// and is audited; each refusal is 400 chapters_invalid with its detail, a basis
// of other files 409 chapters_basis; DELETE reverts it. A member is 403 and an
// anonymous caller 401, and neither changes anything.
func TestAdminChapterEditAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	editURL, chaptersURL := seedTwoFileBook(t, e)
	bookURL := strings.Replace(editURL, "/book/chapters", "/book", 1)

	resp, body := e.do(t, "GET", bookURL, adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_basis":`+twoFileBasis) ||
		strings.Contains(body, `"chapter_edit"`) {
		t.Fatalf("book page: %d %s", resp.StatusCode, body)
	}
	edit := `{"basis":` + twoFileBasis + `,"chapters":[{"start":0,"title":"Prologue"},{"start":900.25,"title":"One"},
		{"start":1800,"title":"Two"},{"start":2700.5,"title":"Three"}]}`

	// Denied: a member, an anonymous caller.
	for _, method := range []string{"PUT", "DELETE"} {
		if resp, body := e.do(t, method, editURL, memberTok, edit); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member %s: %d %s, want 403", method, resp.StatusCode, body)
		}
		if resp, body := e.do(t, method, editURL, "", edit); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d %s, want 401", method, resp.StatusCode, body)
		}
	}
	if _, body := e.do(t, "GET", chaptersURL, adminTok, ""); strings.Contains(body, "chapters_source") {
		t.Fatalf("a refused edit changed the chapters: %s", body)
	}

	// Allowed: the admin.
	resp, body = e.do(t, "PUT", editURL, adminTok, edit)
	var d struct {
		ChapterSource string                 `json:"chapter_source"`
		ChapterEdit   *catalog.ChapterEdit   `json:"chapter_edit"`
		Chapters      []catalog.AdminChapter `json:"chapters"`
	}
	decodeInto(t, body, &d)
	if resp.StatusCode != http.StatusOK || d.ChapterSource != "edited" || d.ChapterEdit == nil ||
		d.ChapterEdit.State != "applied" || d.ChapterEdit.EditedBy == nil || *d.ChapterEdit.EditedBy != "admin" ||
		len(d.Chapters) != 4 || d.Chapters[3].FilePath != "Author/Book/02.mp3" || d.Chapters[3].Start != 900.5 ||
		d.Chapters[3].BookOffset != 2700.5 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", chaptersURL, adminTok, "")
	var ch struct {
		Chapters       []metadata.Chapter `json:"chapters"`
		ChaptersSource string             `json:"chapters_source"`
	}
	decodeInto(t, body, &ch)
	if resp.StatusCode != http.StatusOK || ch.ChaptersSource != "edited" || len(ch.Chapters) != 4 ||
		ch.Chapters[1].Title != "One" || ch.Chapters[1].End != 1800 || ch.Chapters[2].FileIndex != 1 || ch.Chapters[2].BookOffset != 1800 {
		t.Fatalf("player chapters: %d %s", resp.StatusCode, body)
	}
	itemURL := strings.Replace(chaptersURL, "/chapters", "/item", 1)
	if _, body := e.do(t, "GET", itemURL, adminTok, ""); !strings.Contains(body, `"chapters_source":"edited"`) {
		t.Fatalf("item: %s", body)
	}
	events, _, err := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "book"})
	if err != nil || len(events) != 1 || events[0].Action != "book.chapters_edited" || events[0].Target != "Main: Author/Book" {
		t.Fatalf("audit = %+v, %v", events, err)
	}

	chapters := func(list string) string { return `{"basis":` + twoFileBasis + `,"chapters":[` + list + `]}` }
	for name, c := range map[string]struct{ body, detail string }{
		"empty":        {chapters(``), catalog.ChapterEditEmpty},
		"too many":     {chapters(strings.TrimSuffix(strings.Repeat(`{"start":1,"title":"x"},`, 2001), ",")), catalog.ChapterEditTooMany},
		"order":        {chapters(`{"start":0,"title":"A"},{"start":1800,"title":"B"},{"start":1000,"title":"C"}`), catalog.ChapterEditOrder},
		"out of range": {chapters(`{"start":0,"title":"A"},{"start":1800,"title":"B"},{"start":3600,"title":"C"}`), catalog.ChapterEditOutOfRange},
		"title":        {chapters(`{"start":0,"title":" "},{"start":1800,"title":"B"}`), catalog.ChapterEditTitle},
		"file start":   {chapters(`{"start":0,"title":"A"},{"start":1700,"title":"B"}`), catalog.ChapterEditFileStart},
	} {
		resp, body := e.do(t, "PUT", editURL, adminTok, c.body)
		var env struct{ Error, Code, Detail string }
		_ = json.Unmarshal([]byte(body), &env)
		if resp.StatusCode != http.StatusBadRequest || env.Error != "chapters_invalid" || env.Code != "chapters_invalid" || env.Detail != c.detail {
			t.Errorf("%s: %d %s, want 400 chapters_invalid %s", name, resp.StatusCode, body, c.detail)
		}
	}
	other := strings.Replace(edit, `"size":20`, `"size":21`, 1)
	if resp, body := e.do(t, "PUT", editURL, adminTok, other); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"chapters_basis"`) {
		t.Errorf("another basis: %d %s, want 409 chapters_basis", resp.StatusCode, body)
	}
	for name, body := range map[string]string{
		"no basis":      `{"chapters":[{"start":0,"title":"A"}]}`,
		"unknown field": `{"basis":` + twoFileBasis + `,"chapters":[{"start":0,"title":"A","end":1}]}`,
		"not json":      `chapters`,
	} {
		if resp, out := e.do(t, "PUT", editURL, adminTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, out)
		}
	}
	noBook := strings.Replace(editURL, escape("Author/Book"), escape("Nobody/Nothing"), 1)
	for _, method := range []string{"PUT", "DELETE"} {
		if resp, body := e.do(t, method, noBook, adminTok, edit); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s no book: %d %s, want 404", method, resp.StatusCode, body)
		}
	}
	// The refusals left the edit as it was.
	if _, body := e.do(t, "GET", chaptersURL, adminTok, ""); !strings.Contains(body, `"title":"Three"`) {
		t.Fatalf("after the refusals: %s", body)
	}

	// Revert: the files' chapters again, audited once.
	resp, body = e.do(t, "DELETE", editURL, adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_source":"files"`) || strings.Contains(body, `"chapter_edit"`) {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, body)
	}
	if _, body := e.do(t, "GET", chaptersURL, adminTok, ""); strings.Contains(body, "chapters_source") || !strings.Contains(body, `"title":"02"`) {
		t.Fatalf("chapters after revert: %s", body)
	}
	if resp, _ := e.do(t, "DELETE", editURL, adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a second DELETE: %d", resp.StatusCode)
	}
	events, _, err = e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "book"})
	if err != nil || len(events) != 2 || events[0].Action != "book.chapters_reverted" {
		t.Fatalf("audit after revert = %+v, %v", events, err)
	}
}
