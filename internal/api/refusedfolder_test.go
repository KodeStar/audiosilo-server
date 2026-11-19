package api

import (
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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

// TestShareFaultOnAnUnindexedBookIsShareNotAnswering: a book no scan has
// reached, whose folder's listing fails as a share does that a retry may get
// past (EIO, ESTALE: an NFS share that stopped answering, a handle it lost),
// is a 503 share_not_answering with Retry-After to /item's on-demand index and
// to the stream of its track, as the scanner reads the same faults, never the
// 404 "no book at that path" a missing book gets (server#360). Denied the
// 503: a permission on that listing (EACCES, EPERM) stays the 404, and so
// does a book missing from folders that answer; the same asks answer once the
// listing reads again.
func TestShareFaultOnAnUnindexedBookIsShareNotAnswering(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		errno syscall.Errno
		coded bool
	}{
		{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.EACCES, false}, {syscall.EPERM, false},
	} {
		t.Run(c.errno.Error(), func(t *testing.T) {
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
			lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Faulty", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			author := filepath.Join(root, "Author")
			var failing atomic.Bool
			failing.Store(true)
			e.api.listed = metadata.NewDirNames(func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
				if dir == author && failing.Load() {
					return nil, &fs.PathError{Op: "open", Path: dir, Err: c.errno}
				}
				return metadata.ReadDirNamesBounded(ctx, dir, timeout)
			})
			base := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
			asks := []struct{ name, url string }{
				{"/item", base + "/item?path=" + url.QueryEscape("Author/Book")},
				{"the stream", base + "/stream?path=" + url.QueryEscape("Author/Book/01.mp3")},
			}
			for _, a := range asks {
				resp, body := e.do(t, "GET", a.url, adminTok, "")
				coded := resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(body, codeShareNotAnswering) && resp.Header.Get("Retry-After") == "30"
				switch {
				case c.coded && !coded:
					t.Errorf("%s with the folder's listing %v = %d %s (Retry-After %q), want 503 %s with Retry-After 30",
						a.name, c.errno, resp.StatusCode, body, resp.Header.Get("Retry-After"), codeShareNotAnswering)
				case !c.coded && (resp.StatusCode != http.StatusNotFound || resp.Header.Get("Retry-After") != ""):
					t.Errorf("%s with the folder's listing %v = %d %s (Retry-After %q), want a 404 with none",
						a.name, c.errno, resp.StatusCode, body, resp.Header.Get("Retry-After"))
				}
			}
			if resp, body := e.do(t, "GET", base+"/item?path="+url.QueryEscape("Other/Missing"), adminTok, ""); resp.StatusCode != http.StatusNotFound {
				t.Errorf("/item of a missing book on the folders that answer = %d %s, want 404", resp.StatusCode, body)
			}
			failing.Store(false) // a failed listing is never kept: the next ask reads it again
			for _, a := range asks {
				if resp, body := e.do(t, "GET", a.url, adminTok, ""); resp.StatusCode != http.StatusOK {
					t.Errorf("%s once the listing reads = %d %s, want it served", a.name, resp.StatusCode, body)
				}
			}
		})
	}
}
