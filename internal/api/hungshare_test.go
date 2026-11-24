package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/testwait"
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
	// The hold ends while the stream's open waits (spun), the mount lookup
	// put back only once every request is done (restore), so the open's own
	// lookups still name the held folder's mount.
	spun := make(chan struct{})
	var spinOnce sync.Once
	spin := func() { spinOnce.Do(func() { close(spun) }) }
	restore := metadata.HoldOpensUntilForTests(filepath.Join(root, "Book"), spun)
	t.Cleanup(restore)
	t.Cleanup(spin)

	if resp, body := e.do(t, "GET", coverURL(lib.ID, "Book", ""), adminTok, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("the cover while the disk spins up = %d %s, want 503 after fileWait", resp.StatusCode, body)
	}
	time.AfterFunc(spinUp, spin)
	stream := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/stream?path=" + url.QueryEscape("Book/book.m4b")
	if resp, body := e.do(t, "GET", stream, adminTok, ""); resp.StatusCode != http.StatusOK || body != "not really audio" {
		t.Errorf("the stream while the disk spins up = %d %q, want it served once the disk answers", resp.StatusCode, body)
	}
}

// A request still waiting for its book's read when the server stops (/item's
// shared read, an admin's Re-read) is a 503 to try again (the read ended by the
// stop), not a 500 "context canceled". Not parallel:
// metadata.HoldReadsForTests swaps the mount lookup.
func TestBookReadWaitingAtShutdownIs503(t *testing.T) {
	for _, c := range []struct{ name, method, endpoint string }{
		{"item", "GET", "/item"},
		{"re-read", "POST", "/book/rescan"},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			release := metadata.HoldReadsForTests(part) // the book's read waits on its part (its lstat answers)
			t.Cleanup(release)

			type answer struct {
				status int
				body   string
			}
			done := make(chan answer, 1)
			go func() {
				u := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + c.endpoint + "?path=" + url.QueryEscape("Author/Book")
				if c.method == "POST" {
					u = "/api/v1/admin" + strings.TrimPrefix(u, "/api/v1")
				}
				resp, body := e.do(t, c.method, u, adminTok, "")
				done <- answer{resp.StatusCode, body}
			}()
			time.Sleep(300 * time.Millisecond) // the read is under way, held
			stop()
			select {
			case a := <-done:
				if a.status != http.StatusServiceUnavailable || !strings.Contains(a.body, codeStopping) {
					t.Errorf("%s waiting at shutdown = %d %s, want 503 %s", c.endpoint, a.status, a.body, codeStopping)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("%s still waiting after the stop", c.endpoint)
			}
		})
	}
}

// /item of a book whose ebook sits on a share that stopped answering answers
// with the stored book once the look at the ebook is given up on (fileWait),
// rather than waiting out the scan's probe time, which is the request's own
// (a 503 "request timed out" for every book with an ebook). Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestItemWithAnEbookOnAHeldShareAnswers(t *testing.T) {
	e := newEbookEnv(t)
	e.api.fileWait = 200 * time.Millisecond
	release := metadata.HoldOpensForTests(filepath.Join(e.root, filepath.FromSlash(ebookFile)))
	t.Cleanup(release)
	start := time.Now()
	eb, _ := e.itemEbook(t, ebookBook)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("/item took %v, want it answered once the ebook's look is given up on", took)
	}
	if eb == nil || eb.Path != ebookFile {
		t.Errorf("ebook on a held share = %+v, want the stored one", eb)
	}
}

// The reader's ebook on a share that stopped answering answers 503
// share_not_answering with Retry-After once its open is given up on
// (streamWait, as the stream's looks), not a handler held for good; the same
// ask is served once the share answers. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestEbookOnAHeldShareAnswersShareNotAnswering(t *testing.T) {
	e := newEbookEnv(t)
	e.api.streamWait = 200 * time.Millisecond
	release := metadata.HoldOpensForTests(filepath.Join(e.root, filepath.FromSlash(ebookFile)))
	t.Cleanup(release)
	start := time.Now()
	resp, body := e.do(t, "GET", e.at("ebook", ebookBook), e.annTok, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("the ebook on a held share = %d %s, want 503 %s with Retry-After 30", resp.StatusCode, body, codeShareNotAnswering)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the ebook on a held share took %v, want it answered at the open's bound", took)
	}
	release()
	if resp, body := e.do(t, "GET", e.at("ebook", ebookBook), e.annTok, ""); resp.StatusCode != http.StatusOK || len(body) != len(e.epub) {
		t.Errorf("the ebook once the share answers = %d (%d bytes), want the file", resp.StatusCode, len(body))
	}
}

// Every look a stream makes at its file on a share that stopped answering is
// bounded (SRV-15c): the path's join (SafeJoin's Lstats, which ran before the
// open's bound began, adding 71 s to one stream on a frozen share), and the
// open, a transcode's stat, a remux's and a rendition's look at the source
// (the renditions listing's too) each answer 503 share_not_answering with
// Retry-After. The library's root is a link here, so the join (named under
// the link) and the open (under the folder it leads to) are held apart. Not
// parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestStreamLooksOnAHeldShareAnswer(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "lib")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: link})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"book.m4b", "book.mka"} {
		if err := os.WriteFile(filepath.Join(real, "Book", name), []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := &catalog.Book{LibraryID: lib.ID, RelPath: "Book", IsFolder: true, Title: "Book", Format: "m4b",
		Files: []catalog.BookFile{{RelPath: "Book/book.m4b", Seq: 1}}}
	if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	e.api.ffmpeg = "ffmpeg" // never run: each look is given up on first
	e.api.fileWait, e.api.streamWait = 200*time.Millisecond, 200*time.Millisecond
	id := strconv.FormatInt(lib.ID, 10)
	stream := func(rel, query string) string {
		return "/api/v1/libraries/" + id + "/stream?path=" + url.QueryEscape(rel) + query
	}
	urls := []string{
		stream("Book/book.m4b", ""),
		stream("Book/book.m4b", "&transcode=1"),
		stream("Book/book.mka", ""), // remuxed
		stream("Book/book.m4b", "&rendition=watch"),
		"/api/v1/libraries/" + id + "/renditions?path=Book&kind=watch",
	}
	check := func(held string) {
		t.Helper()
		for _, u := range urls {
			start := time.Now()
			resp, body := e.do(t, "GET", u, adminTok, "")
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "30" {
				t.Errorf("GET %s with %s held = %d %s, want 503 %s", u, held, resp.StatusCode, body, codeShareNotAnswering)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("GET %s with %s held took %v, want it answered at the look's bound", u, held, took)
			}
		}
	}

	release := metadata.HoldOpensForTests(link) // the join's Lstats
	t.Cleanup(release)
	check("the join")
	release()

	release = metadata.HoldOpensForTests(filepath.Join(real, "Book")) // the open, the stats
	t.Cleanup(release)
	check("the file")
	release()

	if resp, body := e.do(t, "GET", stream("Book/book.m4b", ""), adminTok, ""); resp.StatusCode != http.StatusOK || body != "not really audio" {
		t.Errorf("the stream once the share answers = %d %q, want it served", resp.StatusCode, body)
	}
}

// An admin's Re-read of a book on a share that stopped answering is a 503
// share_not_answering with Retry-After once a look is given up on (fileWait),
// not the request timeout's generic 503 (SRV-15c): its folder's looks while it
// is resolved, or a file beside its audio while it is read (the book kept as
// stored, that file's problem recorded). Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestRereadOnAHeldShareAnswersShareNotAnswering(t *testing.T) {
	for _, held := range []string{"Author/Book", "Author/Book/cover.jpg"} {
		t.Run(held, func(t *testing.T) {
			e := newTestEnv(t)
			adminTok, _, _ := adminAndMember(t, e)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"01.mp3": make([]byte, 1<<10), "cover.jpg": []byte("img")} {
				p := filepath.Join(root, "Author", "Book", name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Held", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.api.scanner.Scan(t.Context(), *lib); err != nil {
				t.Fatal(err)
			}
			e.api.fileWait = 200 * time.Millisecond
			release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(held)))
			t.Cleanup(release)
			u := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10) + "/book/rescan?path=" + url.QueryEscape("Author/Book")
			start := time.Now()
			resp, body := e.do(t, "POST", u, adminTok, "")
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "30" {
				t.Errorf("Re-read with %s held = %d %s, want 503 %s", held, resp.StatusCode, body, codeShareNotAnswering)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("Re-read took %v, want it answered once a look is given up on", took)
			}
			issues := func() string {
				t.Helper()
				resp, body := e.do(t, "GET", "/api/v1/admin/books?issue=scan_error", adminTok, "")
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("Health = %d %s", resp.StatusCode, body)
				}
				return body
			}
			if got := issues(); !strings.Contains(got, `"scan_error_file":"`+held+`"`) {
				t.Fatalf("Health after re-read of %s = %s", held, got)
			}
			release()
			if resp, body := e.do(t, "POST", u, adminTok, ""); resp.StatusCode != http.StatusOK {
				t.Errorf("Re-read once the share answers = %d %s, want the book", resp.StatusCode, body)
			}
			if got := issues(); strings.Contains(got, `"scan_error":"unreadable"`) {
				t.Fatalf("Health after recovery: %s", got)
			}
		})
	}
}

// A folder listing (/fs) on a share that stopped answering is a quick 503
// share_not_answering, the listing a bounded look (fileWait) like the join
// before it: it was a plain read that held the handler for good. The same
// listing answers once the share does. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestFolderListingOnAHeldShareAnswersQuickly(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Author", "Book", "01.mp3"), []byte("not really audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	e.api.fileWait = 200 * time.Millisecond
	listing := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/fs?path="
	release := metadata.HoldOpensForTests(root)
	t.Cleanup(release)
	start := time.Now()
	resp, body := e.do(t, "GET", listing, adminTok, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) {
		t.Errorf("the root listing on a held share = %d %s, want 503 %s", resp.StatusCode, body, codeShareNotAnswering)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the root listing took %v, want it answered once the look is given up on", took)
	}

	release()
	if resp, body := e.do(t, "GET", listing, adminTok, ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"Author"`) {
		t.Errorf("the root listing once the share answers = %d %s, want the folder listed", resp.StatusCode, body)
	}
}

// A request joining another's read of its book that does not end (/item of a
// book a first /item is reading, its part held) answers 503
// share_not_answering with Retry-After flightWaitMargin ahead of its own
// timeout (a.flightWaitEnd), not the timeout's bare 503 "request timed out",
// which has neither, even when a look's own time (fileWait) is longer than
// the request has. Not parallel: metadata.HoldReadsForTests swaps the mount
// lookup.
func TestJoinedReadEndsBeforeTheRequestTimeout(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	root, lib := unindexedBook(t, e, "Held")
	e.api.timeoutDur = flightWaitMargin + time.Second
	e.api.fileWait = time.Minute // no look given up on before the request times out
	release := metadata.HoldReadsForTests(filepath.Join(root, "Author", "Book", "01.mp3"))
	t.Cleanup(release)
	u := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/item?path=" + url.QueryEscape("Author/Book")
	first := make(chan struct{})
	go func() { // leads the read, held on the part
		defer close(first)
		_, _ = e.do(t, "GET", u, adminTok, "")
	}()
	time.Sleep(300 * time.Millisecond) // the read is under way, held
	resp, body := e.do(t, "GET", u, adminTok, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeShareNotAnswering) || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("/item joining a held read = %d %s (Retry-After %q), want 503 %s with Retry-After 30",
			resp.StatusCode, body, resp.Header.Get("Retry-After"), codeShareNotAnswering)
	}
	release()
	<-first
}

// An admin's Re-read still going when its answer is due (a slow share whose
// looks each answer within fileWait) is a 202 {"still_reading": true}, not the
// request timeout's bare 503, and the read goes on and is saved: the folder,
// held until after the answer, is a book once it is read (server#224). A
// Re-read that finishes in time answers the book's page as before. Not
// parallel: metadata.HoldOpensUntilForTests swaps the mount lookup.
func TestRereadStillReadingIsAccepted(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Author/Quick/01.mp3", "Author/Slow/01.mp3"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, 1<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Slow", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	rescan := func(book string) string {
		return "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10) + "/book/rescan?path=" + url.QueryEscape(book)
	}
	// The answer is due 200 ms + flightWaitMargin/2 in; no look is given up on.
	e.api.timeoutDur = flightWaitMargin + 200*time.Millisecond
	e.api.fileWait = time.Minute

	if resp, body := e.do(t, "POST", rescan("Author/Quick"), memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a member's Re-read = %d %s, want 403", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "POST", rescan("Author/Quick"), adminTok, ""); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"path":"Author/Quick"`) {
		t.Errorf("a quick Re-read = %d %s, want the book's page", resp.StatusCode, body)
	}

	held := make(chan struct{})
	restore := metadata.HoldOpensUntilForTests(filepath.Join(root, "Author", "Slow"), held)
	t.Cleanup(restore)
	var once sync.Once
	answer := func() { once.Do(func() { close(held) }) }
	t.Cleanup(answer)
	resp, body := e.do(t, "POST", rescan("Author/Slow"), adminTok, "")
	if resp.StatusCode != http.StatusAccepted || strings.TrimSpace(body) != `{"still_reading":true}` {
		t.Fatalf("a slow Re-read = %d %s, want 202 still_reading", resp.StatusCode, body)
	}
	if _, err := e.cat.GetBookByPath(t.Context(), lib.ID, "Author/Slow"); err == nil {
		t.Fatal("the slow read was saved before its folder answered")
	}
	answer()
	testwait.Eventually(t, "the slow read saved", 5*time.Second, func() bool {
		_, err := e.cat.GetBookByPath(t.Context(), lib.ID, "Author/Slow")
		return err == nil
	})
}
