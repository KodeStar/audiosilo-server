package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// fourParts is a community chapter list of an hour: four 15-minute parts.
const fourParts = `{"chapters":[{"title":"Part A","start_ms":0,"length_ms":900000},{"title":"Part B","start_ms":900000,"length_ms":900000},{"title":"Part C","start_ms":1800000,"length_ms":900000},{"title":"Part D","start_ms":2700000,"length_ms":900000}]}`

// seedChapterless indexes an hour-long single-file book with an ASIN and no
// chapters, returning the library id.
func seedChapterless(t *testing.T, e *testEnv, path string) int64 {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "Book",
		Duration: 3600, ASIN: "B00B5HZGUG", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	return lib.ID
}

type bookPage struct {
	ChapterSource     string `json:"chapter_source"`
	ChapterChoice     string `json:"chapter_choice"`
	CommunityChecking bool   `json:"community_checking"`
	CommunityChapters *struct {
		Status   string `json:"status"`
		Chapters []struct {
			Title string `json:"title"`
		} `json:"chapters"`
	} `json:"community_chapters"`
	Chapters []struct {
		Title string `json:"title"`
	} `json:"chapters"`
}

func TestCommunityChaptersCheck(t *testing.T) {
	t.Parallel()
	e := newMetaEnvMock(t, true, &mockMetaserve{chapters: fourParts})
	const p = "Andy Weir/The Martian.m4b"
	libID := seedChapterless(t, e, p)
	admin, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	book := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book?path=" + escape(p)
	check := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book/community-chapters?path=" + escape(p)

	// Denied: a listener can't start a check.
	user, _ := e.auth.CreateUser(context.Background(), "listener", "listener-password", auth.RoleUser)
	userTok, _ := e.auth.IssueToken(context.Background(), user.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "POST", check, userTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("listener check = %d, want 403", resp.StatusCode)
	}

	resp, body := e.do(t, "POST", check, admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check = %d %s", resp.StatusCode, body)
	}
	var page bookPage
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body = e.do(t, "GET", book, admin, "")
		page = bookPage{}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		if !page.CommunityChecking && page.CommunityChapters != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the check never finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if page.CommunityChapters.Status != "fill" || page.ChapterSource != "community" || page.ChapterChoice != "" || len(page.Chapters) != 4 {
		t.Fatalf("after the check: %s", body)
	}

	// The player's chapters say where they came from; an old client reads the
	// same envelope as ever.
	chapters := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/chapters?path=" + escape(p)
	_, body = e.do(t, "GET", chapters, admin, "")
	if !strings.Contains(body, `"chapters_source":"community"`) || !strings.Contains(body, `"title":"Part C"`) {
		t.Fatalf("player chapters: %s", body)
	}

	// The admin picks the files' chapters (none): the community ones go.
	resp, body = e.do(t, "PATCH", book, admin, `{"chapter_source":"files"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_choice":"files"`) || !strings.Contains(body, `"chapter_source":"files"`) {
		t.Fatalf("pick files = %d %s", resp.StatusCode, body)
	}
	_, body = e.do(t, "GET", chapters, admin, "")
	if strings.Contains(body, "chapters_source") || !strings.Contains(body, `"chapters":[]`) {
		t.Fatalf("player chapters after picking files: %s", body)
	}
	if resp, body = e.do(t, "PATCH", book, admin, `{"chapter_source":"tags"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad source = %d %s, want 400", resp.StatusCode, body)
	}
}

func TestCommunityChaptersCheckMetadataOff(t *testing.T) {
	t.Parallel()
	e := newMetaEnvMock(t, false, &mockMetaserve{chapters: fourParts})
	libID := seedChapterless(t, e, "A/B.m4b")
	admin, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	resp, body := e.do(t, "POST", "/api/v1/admin/libraries/"+strconv.FormatInt(libID, 10)+"/book/community-chapters?path="+escape("A/B.m4b"), admin, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "metadata_off") {
		t.Fatalf("check with metadata off = %d %s, want 404 metadata_off", resp.StatusCode, body)
	}
}
