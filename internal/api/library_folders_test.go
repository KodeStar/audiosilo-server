package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
