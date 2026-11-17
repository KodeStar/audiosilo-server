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
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A cover or stream request for a file on a share that stopped answering
// (SRV-15b: the handler's open and tag read held it for good, the Health page's
// thumbnails tying up handlers) answers quickly: 503 share_not_answering once
// the look is given up on, the full cover, its thumbnail and the stream alike.
// Art the share doesn't hold answers as ever. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestCoverOnAHeldShareAnswersQuickly(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	// The root as the stream's SafeJoin resolves it (macOS's /var is a link),
	// so the held folder is the one every look names.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	libID := lib.ID
	for _, b := range []*catalog.Book{{RelPath: "Sidecar", CoverPath: "Sidecar/cover.jpg"}, {RelPath: "Custom"}} {
		if err := os.MkdirAll(filepath.Join(root, b.RelPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, b.RelPath, "book.m4b"), []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		b.LibraryID, b.IsFolder, b.Title, b.Format = lib.ID, true, b.RelPath, "m4b"
		b.Files = []catalog.BookFile{{RelPath: b.RelPath + "/book.m4b", Seq: 1}}
		if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Sidecar", "cover.jpg"), testImage(t, 800, 800, false), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.SetCover(context.Background(), lib.ID, "Custom", testImage(t, 1000, 500, true), e.adminID, catalog.SourceEdited); err != nil {
		t.Fatal(err)
	}
	e.api.fileWait = 200 * time.Millisecond
	release := metadata.HoldOpensForTests(filepath.Join(root, "Sidecar"))
	t.Cleanup(release)

	stream := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/stream?path=" + url.QueryEscape("Sidecar/book.m4b")
	for _, u := range []string{coverURL(libID, "Sidecar", ""), coverURL(libID, "Sidecar", "96"), stream} {
		start := time.Now()
		resp, body := e.do(t, "GET", u, adminTok, "")
		if resp.StatusCode != http.StatusServiceUnavailable || (!strings.Contains(u, "/stream") && !strings.Contains(body, codeShareNotAnswering)) {
			t.Errorf("GET %s on a held share = %d %s, want 503 %s", u, resp.StatusCode, body, codeShareNotAnswering)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("GET %s took %v, want it answered once the look is given up on", u, took)
		}
	}
	if resp, body := e.do(t, "GET", coverURL(libID, "Custom", ""), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("a custom cover beside the held share = %d %s, want it served", resp.StatusCode, body)
	}

	release()
	if resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", ""), adminTok, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Cache-Control") != coverCache || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("the sidecar once the share answers = %d %s (%q, %q), want the image, cached a day",
			resp.StatusCode, body, resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Type"))
	}
}

// An /item request still waiting for its book's read when the server stops is
// a 503 to try again (the read ended by the stop), not a 500 "context
// canceled". Not parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestItemWaitingAtShutdownIs503(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	root := t.TempDir()
	part := filepath.Join(root, "Author", "Book", "01.mp3")
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, make([]byte, 1<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Held", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	base, stop := context.WithCancel(context.Background())
	defer stop()
	e.api.SetBaseContext(base)
	release := metadata.HoldOpensForTests(part) // the book's read waits on its part
	t.Cleanup(release)

	type answer struct {
		status int
		body   string
	}
	done := make(chan answer, 1)
	go func() {
		resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/item?path="+url.QueryEscape("Author/Book"), adminTok, "")
		done <- answer{resp.StatusCode, body}
	}()
	time.Sleep(300 * time.Millisecond) // the read is under way, held
	stop()
	select {
	case a := <-done:
		if a.status != http.StatusServiceUnavailable || !strings.Contains(a.body, codeStopping) {
			t.Errorf("/item waiting at shutdown = %d %s, want 503 %s", a.status, a.body, codeStopping)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("/item still waiting after the stop")
	}
}
