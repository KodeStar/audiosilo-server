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
	e.api.fileWait, e.api.streamWait = 200*time.Millisecond, 200*time.Millisecond
	release := metadata.HoldOpensForTests(filepath.Join(root, "Sidecar"))
	t.Cleanup(release)

	stream := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/stream?path=" + url.QueryEscape("Sidecar/book.m4b")
	for _, u := range []string{coverURL(libID, "Sidecar", ""), coverURL(libID, "Sidecar", "96"), stream} {
		start := time.Now()
		resp, body := e.do(t, "GET", u, adminTok, "")
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "30" {
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

// A stream's open waits longer than a cover's look (streamWait, not fileWait):
// a disk spinning up answers after a cover has given up, and the stream that
// waited for it plays rather than answering a 503 the player won't retry. The
// cover on the same held share is still a quick 503. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestStreamWaitsOutADiskSpinningUp(t *testing.T) {
	if !(fileWaitTimeout < streamOpenTimeout && streamOpenTimeout < 60*time.Second) {
		t.Errorf("streamOpenTimeout %v, want it past fileWaitTimeout (%v) and under AVPlayer's 60 s", streamOpenTimeout, fileWaitTimeout)
	}
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	b := &catalog.Book{LibraryID: lib.ID, RelPath: "Book", IsFolder: true, Title: "Book", Format: "m4b", CoverPath: "Book/cover.jpg",
		Files: []catalog.BookFile{{RelPath: "Book/book.m4b", Seq: 1}}}
	if err := os.MkdirAll(filepath.Join(root, "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Book", "book.m4b"), []byte("not really audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Book", "cover.jpg"), testImage(t, 100, 100, false), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	const spinUp = 600 * time.Millisecond
	e.api.fileWait, e.api.streamWait = 200*time.Millisecond, 10*time.Second
	release := metadata.HoldOpensForTests(filepath.Join(root, "Book"))
	t.Cleanup(release)

	if resp, body := e.do(t, "GET", coverURL(lib.ID, "Book", ""), adminTok, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("the cover while the disk spins up = %d %s, want 503 after fileWait", resp.StatusCode, body)
	}
	time.AfterFunc(spinUp, release)
	stream := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/stream?path=" + url.QueryEscape("Book/book.m4b")
	if resp, body := e.do(t, "GET", stream, adminTok, ""); resp.StatusCode != http.StatusOK || body != "not really audio" {
		t.Errorf("the stream while the disk spins up = %d %q, want it served once the disk answers", resp.StatusCode, body)
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
