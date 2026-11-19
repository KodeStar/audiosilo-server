package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A book folder the server's user may not read (a permission: its own, or
// the folder's above it) is no share that stopped answering: the refusal
// stays until an admin changes it, so a book asked for there, by a browsing
// client (/item, not indexed yet) or an admin's Re-read, keeps the lasting
// answer it had before a share's faults were read as looks given up on, a 404
// with nothing to try again, never a 503 share_not_answering with
// Retry-After (server#360). The same asks answer once the folder is readable.
func TestRefusedBookFolderIsNoShareNotAnswering(t *testing.T) {
	t.Parallel()
	for _, refused := range []string{"Author/Book", "Author"} {
		t.Run(refused, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t)
			adminTok, _, _ := adminAndMember(t, e)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "Author", "Book", "01.mp3"), make([]byte, 1<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Refused", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, filepath.FromSlash(refused))
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, err := os.ReadDir(dir); err == nil {
				t.Skip("folder permissions don't stop this user reading (root?)")
			}
			id := strconv.FormatInt(lib.ID, 10)
			q := "?path=" + url.QueryEscape("Author/Book")
			asks := []struct{ name, method, url string }{
				{"/item", "GET", "/api/v1/libraries/" + id + "/item" + q},
				{"Re-read", "POST", "/api/v1/admin/libraries/" + id + "/book/rescan" + q},
			}
			for _, a := range asks {
				resp, body := e.do(t, a.method, a.url, adminTok, "")
				if resp.StatusCode != http.StatusNotFound || strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "" {
					t.Errorf("%s with %s refused = %d %s (Retry-After %q), want a 404 with none", a.name, refused, resp.StatusCode, body, resp.Header.Get("Retry-After"))
				}
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, a := range asks {
				if resp, body := e.do(t, a.method, a.url, adminTok, ""); resp.StatusCode != http.StatusOK {
					t.Errorf("%s once the folder is readable = %d %s, want the book", a.name, resp.StatusCode, body)
				}
			}
		})
	}
}
