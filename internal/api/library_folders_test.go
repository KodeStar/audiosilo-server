package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// keepFolder reads a file under root as a scan does, so its folder is kept
// open for the next read, and reports whether it is (another test letting
// every folder go meanwhile is tried again).
func keepFolder(t *testing.T, root string) bool {
	t.Helper()
	file := filepath.Join(root, "Book", "01.mp3")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := metadata.StatBelow(root, file); err != nil {
			t.Fatal(err)
		}
		if metadata.FoldersKeptForTests(root) > 0 {
			return true
		}
	}
	return false
}

// A library removed, or moved to another root, keeps none of its folders
// open for a next read: the share it was on can be unmounted at once.
func TestLibraryGoneKeepsNoFolderOpen(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no folder is kept past its read on Windows")
	}
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	for _, tc := range []struct {
		name   string
		method string
		body   func() string
		want   int
	}{
		{"removed", "DELETE", func() string { return "" }, http.StatusNoContent},
		{"moved", "PATCH", func() string { return `{"root":` + strconv.Quote(t.TempDir()) + `}` }, http.StatusOK},
	} {
		root := t.TempDir()
		lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: tc.name, Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if !keepFolder(t, root) {
			t.Fatalf("%s: no folder kept open after a read", tc.name)
		}
		resp, body := e.do(t, tc.method, "/api/v1/admin/libraries/"+strconv.FormatInt(lib.ID, 10), adminTok, tc.body())
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
		}
		if n := metadata.FoldersKeptForTests(root); n != 0 {
			t.Errorf("%s: %d of its folders still kept open", tc.name, n)
		}
	}
}

// The console can read the folder and all its file lengths directly by path.
// A listener's listing omits the admin detail even when asked for it.
func TestBrowseFolderDetail(t *testing.T) {
	e := newTestEnv(t)
	admin, member, memberID := adminAndMember(t, e)
	root := t.TempDir()
	const folder = "Late"
	if err := os.Mkdir(filepath.Join(root, folder), 0o755); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(t.Context(), catalog.Library{Name: "Large", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	b := catalog.Book{LibraryID: lib.ID, RelPath: folder, IsFolder: true, Title: "A long book"}
	for i := range 600 {
		file := fmt.Sprintf("Late/Part%03d.mp3", i)
		if err := os.WriteFile(filepath.Join(root, file), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		b.Files = append(b.Files, catalog.BookFile{RelPath: file, Seq: i, Duration: 5, Size: 4})
	}
	if _, err = e.cat.UpsertBook(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/api/v1/libraries/%d/fs?path=Late&detail=1&limit=500", lib.ID)
	page := func(tok string, offset int) library.Listing {
		t.Helper()
		resp, body := e.do(t, "GET", url+fmt.Sprintf("&offset=%d", offset), tok, "")
		var result library.Listing
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil {
			t.Fatalf("page %d = %d %s", offset, resp.StatusCode, body)
		}
		return result
	}
	first := page(admin, 0)
	if first.Folder == nil || first.Folder.Path != folder || !first.Folder.IsBook || first.Folder.Title != b.Title || first.NextOffset != 500 {
		t.Fatalf("folder %+v, next %d", first.Folder, first.NextOffset)
	}
	second := page(admin, first.NextOffset)
	var bytes int64
	var seconds float64
	for _, f := range append(first.Entries, second.Entries...) {
		bytes += f.Size
		seconds += f.Duration
	}
	if first.Total != 600 || len(first.Entries) != 500 || len(second.Entries) != 100 || bytes != 2400 || seconds != 3000 {
		t.Fatalf("two pages: total %d, sizes %d and %d, bytes %d, seconds %f", first.Total, len(first.Entries), len(second.Entries), bytes, seconds)
	}
	if err := e.cat.GrantWholeLibrary(t.Context(), memberID, lib.ID); err != nil {
		t.Fatal(err)
	}
	listener := page(member, 0)
	if listener.Folder != nil || listener.Entries[0].Duration != 0 {
		t.Fatal("listener received the admin detail")
	}
}
