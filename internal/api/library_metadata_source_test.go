package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A library's metadata source: validated with a code, admin-only, carried by the
// admin list (not the player's), and a change re-resolves the books at once with no
// rescan queued.
func TestLibraryMetadataSourceEndpoint(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	const p = "James S. A. Corey/The Expanse/03 - Abaddon's Gate"
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: p, IsFolder: true,
		Title: "Abaddon's Gate: Expanse, Book 3", Author: "James S. A. Corey", Series: "The Expanse, book #3",
		AddedAt: "2020-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10)

	if resp, body := e.do(t, "PATCH", url, adminTok, `{"metadata_source":"folders"}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, `"code":"invalid_metadata_source"`) {
		t.Fatalf("bad source = %d %s, want 400 invalid_metadata_source", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "PATCH", url, memberTok, `{"metadata_source":"path"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member PATCH = %d, want 403", resp.StatusCode)
	}
	if b, _ := e.cat.GetBookByPath(ctx, lib.ID, p); b.Title != "Abaddon's Gate: Expanse, Book 3" {
		t.Fatalf("a refused PATCH changed the book: %q", b.Title)
	}

	if resp, body := e.do(t, "PATCH", url, adminTok, `{"metadata_source":"path"}`); resp.StatusCode != 200 ||
		strings.Contains(body, `"job"`) {
		t.Fatalf("source PATCH = %d %s, want 200 and no job", resp.StatusCode, body)
	}
	if _, queued := e.api.scanner.Jobs(); len(queued) != 0 {
		t.Fatalf("a metadata source change queued a scan: %+v", queued)
	}
	if b, _ := e.cat.GetBookByPath(ctx, lib.ID, p); b.Title != "Abaddon's Gate" || b.Series != "The Expanse" || b.SeriesIndex != 3 {
		t.Fatalf("book after the switch = %q / %q #%v, want the path's", b.Title, b.Series, b.SeriesIndex)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/libraries", adminTok, ""); !strings.Contains(body, `"metadata_source":"path"`) {
		t.Fatalf("admin libraries missing the source: %s", body)
	}
	if _, body := e.do(t, "GET", "/api/v1/libraries", adminTok, ""); strings.Contains(body, "metadata_source") {
		t.Fatalf("the player's library list carries the source: %s", body)
	}
}
