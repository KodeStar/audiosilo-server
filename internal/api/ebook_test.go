package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// ebookEnv: a library (lib) of books with and without an ebook, scanned; ann
// may read all of it, kid only "Author/Two".
type ebookEnv struct {
	*testEnv
	lib    *catalog.Library
	root   string
	epub   []byte
	ann    int64
	annTok string
	kid    int64
	kidTok string
}

// The ebook library's books.
const (
	ebookBook  = "Author/Book"           // a folder book with one readable epub
	ebookFile  = "Author/Book/Book.epub" // its ebook
	ebookTrack = "Author/Book/01.m4b"
	twoEpubs   = "Author/Two"   // two epubs: none
	plainBook  = "Author/Plain" // no epub
	goneBook   = "Author/Gone"  // its epub removed after the scan
	looseBook  = "Loose.m4b"    // a single-file book with Loose.epub beside it
	linkedBook = "Linked.m4b"   // Linked.epub beside it links out of the library
)

func newEbookEnv(t *testing.T) *ebookEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := copyFixtures(t)
	for _, book := range []string{ebookBook, twoEpubs, plainBook, goneBook} {
		copyFixture(t, root, cradlePart, book+"/01.m4b")
	}
	copyFixture(t, root, cradlePart, looseBook)
	copyFixture(t, root, cradlePart, linkedBook)
	epub := testEpub(t, false)
	put := func(rel string, body []byte) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(ebookFile, epub)
	put("Author/Book/.hidden.epub", epub)
	put("Author/Book/.audiosilo/characters.json", []byte(`{}`))
	put(twoEpubs+"/a.epub", epub)
	put(twoEpubs+"/b.epub", epub)
	put(goneBook+"/Gone.epub", epub)
	put("Loose.epub", epub)
	away := filepath.Join(t.TempDir(), "Away.epub")
	if err := os.WriteFile(away, epub, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, filepath.Join(root, "Linked.epub")); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Ebooks", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	env := &ebookEnv{testEnv: e, lib: lib, root: root, epub: epub}
	env.ann, env.annTok = newUserWithSession(t, e, "ann", false)
	env.kid, env.kidTok = newUserWithSession(t, e, "kid", false)
	if err := e.cat.GrantWholeLibrary(ctx, env.ann, lib.ID); err != nil {
		t.Fatal(err)
	}
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Two only",
		Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: twoEpubs}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, env.kid, share.ID); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *ebookEnv) at(endpoint, path string) string { return bookURL(e.lib.ID, endpoint, path) }

// itemEbook is the ebook GET /item reports for path (nil for none), and whether
// the key was there at all.
func (e *ebookEnv) itemEbook(t *testing.T, path string) (*catalog.Ebook, bool) {
	t.Helper()
	resp, body := e.do(t, "GET", e.at("item", path), e.annTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item %s = %d %s", path, resp.StatusCode, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	field, ok := raw["ebook"]
	if !ok {
		return nil, false
	}
	var eb catalog.Ebook
	if err := json.Unmarshal(field, &eb); err != nil {
		t.Fatal(err)
	}
	return &eb, true
}

// A book with one readable epub carries ebook {path, size} on every Book; one
// with two, none, or one leading out of the library carries no key at all.
func TestBookEbookField(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	for path, want := range map[string]string{ebookBook: ebookFile, looseBook: "Loose.epub",
		twoEpubs: "", plainBook: "", linkedBook: ""} {
		eb, present := e.itemEbook(t, path)
		switch {
		case want == "" && present:
			t.Errorf("%s: ebook = %+v, want no key", path, eb)
		case want != "" && (eb == nil || eb.Path != want || eb.Size != int64(len(e.epub))):
			t.Errorf("%s: ebook = %+v, want {%s %d}", path, eb, want, len(e.epub))
		}
	}
	// Lists carry it too.
	resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/books", e.lib.ID), e.annTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ebook":{"path":"`+ebookFile+`","size":`) {
		t.Fatalf("books list = %d %s", resp.StatusCode, body)
	}
}

// The console's book page and book list carry the same ebook {path, size} as
// the player's Book (ADMIN-01), and no key for a book without one.
func TestAdminBookEbookField(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	adminTok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{ebookBook: ebookFile, looseBook: "Loose.epub", plainBook: ""} {
		resp, body := e.do(t, "GET", "/api/v1/admin"+strings.TrimPrefix(e.at("book", path), "/api/v1"), adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin book %s = %d %s", path, resp.StatusCode, body)
		}
		var d struct {
			Book map[string]json.RawMessage `json:"book"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		field, present := d.Book["ebook"]
		if want == "" {
			if present {
				t.Errorf("%s: ebook = %s, want no key", path, field)
			}
			continue
		}
		var eb catalog.Ebook
		if err := json.Unmarshal(field, &eb); err != nil || eb.Path != want || eb.Size != int64(len(e.epub)) {
			t.Errorf("%s: ebook = %s, want {%s %d}", path, field, want, len(e.epub))
		}
	}
	resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/admin/books?library_id=%d", e.lib.ID), adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ebook":{"path":"`+ebookFile+`","size":`) {
		t.Fatalf("admin books list = %d %s", resp.StatusCode, body)
	}
}

// Allowed: the book's own path with a session or an API key in the header gets
// the epub's bytes as application/epub+zip with a sandboxing policy, Range,
// validators and no shared caching.
func TestEbookServed(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	key, err := e.auth.IssueToken(context.Background(), e.ann, auth.KindAPI, "script", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{e.annTok, key} {
		resp, body := e.do(t, "GET", e.at("ebook", ebookBook), tok, "")
		if resp.StatusCode != http.StatusOK || body != string(e.epub) {
			t.Fatalf("GET ebook = %d (%d bytes), want the epub", resp.StatusCode, len(body))
		}
		h := resp.Header
		for name, want := range map[string]string{
			"Content-Type":                 "application/epub+zip",
			"Content-Security-Policy":      "sandbox; default-src 'none'",
			"Cache-Control":                "private, no-cache",
			"X-Content-Type-Options":       "nosniff",
			"Cross-Origin-Resource-Policy": "same-site",
			"Accept-Ranges":                "bytes",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if h.Get("ETag") == "" || h.Get("Last-Modified") == "" {
			t.Errorf("no validators: %v", h)
		}
	}
	resp, body := e.doHeaders(t, "GET", e.at("ebook", ebookBook), e.annTok, "", map[string]string{"Range": "bytes=0-9"})
	if resp.StatusCode != http.StatusPartialContent || body != string(e.epub[:10]) {
		t.Fatalf("range = %d %q", resp.StatusCode, body)
	}
	full, _ := e.do(t, "GET", e.at("ebook", ebookBook), e.annTok, "")
	if resp, _ := e.doHeaders(t, "GET", e.at("ebook", ebookBook), e.annTok, "",
		map[string]string{"If-None-Match": full.Header.Get("ETag")}); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", resp.StatusCode)
	}
	// A single-file book's ebook beside it.
	if resp, body := e.do(t, "GET", e.at("ebook", looseBook), e.annTok, ""); resp.StatusCode != http.StatusOK || body != string(e.epub) {
		t.Fatalf("single-file book's ebook = %d", resp.StatusCode)
	}
}

// Denied: never by ?token= (a session's or a cast token's), never a cast token
// in the header; outside the caller's scope 403; anything but a book's own path
// (a track, the epub itself, a hidden or .audiosilo/ path), a book with no
// ebook, two, one leading out of the library or one removed since the scan 404.
func TestEbookDenied(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	q := e.at("ebook", ebookBook)
	if resp, _ := e.do(t, "GET", q+"&token="+url.QueryEscape(e.annTok), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("?token= = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", q, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", resp.StatusCode)
	}
	resp, body := e.do(t, "POST", "/api/v1/cast/token", e.annTok,
		fmt.Sprintf(`{"library_id":%d,"path":%q}`, e.lib.ID, ebookBook))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint cast token = %d %s", resp.StatusCode, body)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do(t, "GET", q+"&token="+url.QueryEscape(minted.Token), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a cast token as ?token= = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", q, minted.Token, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a cast token in the header = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", q, e.kidTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outside kid's scope = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/ebook", e.lib.ID), e.annTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no path = %d, want 400", resp.StatusCode)
	}
	if err := os.Remove(filepath.Join(e.root, "Author", "Gone", "Gone.epub")); err != nil {
		t.Fatal(err)
	}
	// A folder book's ebook swapped for a link since the scan (to a file inside
	// the library) is no ebook either.
	swapped := filepath.Join(e.root, filepath.FromSlash(ebookFile))
	if err := os.Remove(swapped); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.root, "Loose.epub"), swapped); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ebookBook, ebookTrack, ebookFile, "Author/Book/.hidden.epub", "Author/Book/.audiosilo/characters.json",
		"Author/Book/.audiosilo", "Author", plainBook, twoEpubs, linkedBook, goneBook, "Author/Missing"} {
		if resp, body := e.do(t, "GET", e.at("ebook", p), e.annTok, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET ebook %s = %d %.60s, want 404", p, resp.StatusCode, body)
		}
	}
	// The epub is no audio: /stream never serves it, with a header or ?token=.
	if resp, _ := e.do(t, "GET", e.at("stream", ebookFile), e.annTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stream of the epub = %d, want 404", resp.StatusCode)
	}
	// kid's own book has two epubs: a 404 inside scope, no bytes.
	if resp, _ := e.do(t, "GET", e.at("ebook", twoEpubs), e.kidTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("kid's book with two epubs = %d, want 404", resp.StatusCode)
	}
}

// An ebook that is there but can't be read right now (a permission, a flaky
// mount) is a 500, logged, never 404 "this book has no ebook", which the player
// would show as gone until the next scan.
func TestEbookUnreadableIs500(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind (not root)")
	}
	t.Parallel()
	e := newEbookEnv(t)
	p := filepath.Join(e.root, filepath.FromSlash(ebookFile))
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if resp, body := e.do(t, "GET", e.at("ebook", ebookBook), e.annTok, ""); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an unreadable ebook = %d %.60s, want 500", resp.StatusCode, body)
	}
}

// The ebook's bytes outlive the request timeout, but its lookup doesn't: with the
// one writer held (a stalled write), a book indexed on demand is answered at the
// request timeout instead of waiting for as long as the writer is held.
func TestEbookLookupBounded(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	ctx := context.Background()
	e.api.timeoutDur = 300 * time.Millisecond
	// The caller's token was just used (its last-seen touch, a write, is skipped
	// for a minute), and the book isn't indexed: the lookup indexes it on demand.
	if resp, _ := e.do(t, "GET", e.at("item", looseBook), e.annTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm up = %d", resp.StatusCode)
	}
	if err := e.cat.DeleteBookAt(ctx, e.lib.ID, ebookBook); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	stalled := make(chan struct{})
	go func() {
		defer close(stalled)
		_ = e.db.WithTx(ctx, "stall", func(*sql.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t.Cleanup(func() { close(release); <-stalled })
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", e.srv.URL+e.at("ebook", ebookBook), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.annTok)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET ebook with the writer held: %v (want an answer at the request timeout)", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET ebook with the writer held = %d, want 503 like any timed-out lookup", resp.StatusCode)
	}
}

// Denied: a podcast episode has no ebook and no reading progress.
func TestEbookPodcastRefused(t *testing.T) {
	t.Parallel()
	b, _, f := podcastEnv(t, false)
	episode := f.Episodes[0].Path
	for _, req := range []struct{ method, endpoint, body string }{
		{"GET", "ebook", ""}, {"GET", "reading-progress", ""},
		{"PUT", "reading-progress", `{"cfi":"epubcfi(/6/2!)","progression":0.1,"device_id":"d","updated_at":"2026-10-25T10:00:00Z"}`},
	} {
		resp, body := b.doHeaders(t, req.method, bookURL(b.lib.ID, req.endpoint, episode), b.userTok, req.body, podcastHeaders())
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s on an episode = %d %s, want 404", req.method, req.endpoint, resp.StatusCode, body)
		}
	}
}

// readingPut PUTs body as tok to reading-progress at path and decodes the answer.
func (e *ebookEnv) readingPut(t *testing.T, tok, path, body string) (rp map[string]any, other map[string]any) {
	t.Helper()
	resp, out := e.do(t, "PUT", e.at("reading-progress", path), tok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT reading-progress %s = %d %s", body, resp.StatusCode, out)
	}
	var got struct {
		ReadingProgress map[string]any `json:"reading_progress"`
		OtherPlace      map[string]any `json:"other_place"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	return got.ReadingProgress, got.OtherPlace
}

func keysOf(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// The reading-progress routes: the wire shape, a part path kept on its book,
// latest-wins for a save that saw the row (and a stale save echoed), Sync v2's
// divergence (the further place kept, the other offered) both ways, the same
// device diverging only on base 0 (saw nothing), and the device's name always
// the token's.
func TestReadingProgressEndpoints(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	if resp, body := e.do(t, "GET", e.at("reading-progress", ebookBook), e.annTok, ""); resp.StatusCode != http.StatusOK ||
		strings.TrimSpace(body) != `{"reading_progress":null}` {
		t.Fatalf("nothing saved = %d %s", resp.StatusCode, body)
	}
	// Through a track's path, stored on the book's; the body's device_name is ignored.
	rp, other := e.readingPut(t, e.annTok, ebookTrack, `{"cfi":"epubcfi(/6/4!/4/2/1:0)","progression":0.2,`+
		`"device_id":"phone","device_name":"Spoofed","updated_at":"2026-10-25T10:00:00Z","base_version":0}`)
	if other != nil || keysOf(rp) != "cfi,device_id,device_name,library_id,path,progression,updated_at,version" ||
		rp["path"] != ebookBook || rp["version"] != 1.0 || rp["device_name"] != "ann's phone" ||
		rp["updated_at"] != "2026-10-25T10:00:00.000Z" || rp["library_id"] != float64(e.lib.ID) {
		t.Fatalf("first save = %v, other %v", rp, other)
	}
	resp, body := e.do(t, "GET", e.at("reading-progress", ebookBook), e.annTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"progression":0.2`) || !strings.Contains(body, `"device_name":"ann's phone"`) {
		t.Fatalf("GET after save = %d %s", resp.StatusCode, body)
	}

	// Not diverged: the same device reads on, on the version it saw.
	rp, other = e.readingPut(t, e.annTok, ebookBook, `{"cfi":"epubcfi(/6/8!/4/2/1:0)","progression":0.4,`+
		`"device_id":"phone","updated_at":"2026-10-25T10:05:00Z","base_version":1}`)
	if other != nil || rp["version"] != 2.0 || rp["progression"] != 0.4 {
		t.Fatalf("same device = %v, other %v", rp, other)
	}
	// Not diverged: another device that saw version 2 moves back on purpose.
	key, err := e.auth.IssueToken(context.Background(), e.ann, auth.KindAPI, "script", 0)
	if err != nil {
		t.Fatal(err)
	}
	rp, other = e.readingPut(t, key, ebookBook, `{"cfi":"epubcfi(/6/2!/4/2/1:0)","progression":0.1,`+
		`"device_id":"web","updated_at":"2026-10-25T10:06:00Z","base_version":2}`)
	if other != nil || rp["version"] != 3.0 || rp["progression"] != 0.1 || rp["device_name"] != "script" {
		t.Fatalf("deliberate move back = %v, other %v", rp, other)
	}
	// A stale save that saw the row: the stored row echoed, nothing offered.
	rp, other = e.readingPut(t, e.annTok, ebookBook, `{"cfi":"epubcfi(/6/20!/4/2/1:0)","progression":0.9,`+
		`"device_id":"phone","updated_at":"2026-10-25T10:01:00Z","base_version":3}`)
	if other != nil || rp["version"] != 3.0 || rp["progression"] != 0.1 {
		t.Fatalf("stale save = %v, other %v", rp, other)
	}
	// No base_version sent: latest-wins too.
	rp, other = e.readingPut(t, e.annTok, ebookBook, `{"cfi":"epubcfi(/6/6!/4/2/1:0)","progression":0.3,`+
		`"device_id":"phone","updated_at":"2026-10-25T10:07:00Z"}`)
	if other != nil || rp["version"] != 4.0 || rp["progression"] != 0.3 {
		t.Fatalf("no base = %v, other %v", rp, other)
	}

	// Diverged, behind: phone saved version 4; web saw only 3 and is behind.
	rp, other = e.readingPut(t, key, ebookBook, `{"cfi":"epubcfi(/6/4!/4/2/1:0)","progression":0.25,`+
		`"device_id":"web","updated_at":"2026-10-25T10:08:00Z","base_version":3}`)
	if rp["version"] != 4.0 || rp["progression"] != 0.3 || rp["device_id"] != "phone" ||
		other == nil || keysOf(other) != "cfi,device_id,device_name,progression,updated_at" ||
		other["progression"] != 0.25 || other["device_id"] != "web" || other["device_name"] != "script" {
		t.Fatalf("diverged behind = %v, other %v", rp, other)
	}
	// Diverged, further: applied, phone's place offered back.
	rp, other = e.readingPut(t, key, ebookBook, `{"cfi":"epubcfi(/6/14!/4/2/1:0)","progression":0.7,`+
		`"device_id":"web","updated_at":"2026-10-25T10:09:00Z","base_version":3}`)
	if rp["version"] != 5.0 || rp["progression"] != 0.7 || rp["device_id"] != "web" ||
		other == nil || other["progression"] != 0.3 || other["device_id"] != "phone" || other["device_name"] != "ann's phone" ||
		other["cfi"] != "epubcfi(/6/6!/4/2/1:0)" {
		t.Fatalf("diverged further = %v, other %v", rp, other)
	}

	// The same device on base 0 saw nothing (signed out and back in, its places
	// there cleared): diverged from its own row too, so its start never replaces
	// a place further on. web's 0.7 stays, the start comes back as other_place.
	rp, other = e.readingPut(t, key, ebookBook, `{"cfi":"epubcfi(/6/2!/4/2/1:0)","progression":0.01,`+
		`"device_id":"web","updated_at":"2026-10-25T11:00:00Z","base_version":0}`)
	if rp["version"] != 5.0 || rp["progression"] != 0.7 || rp["device_id"] != "web" ||
		other == nil || other["progression"] != 0.01 || other["device_id"] != "web" {
		t.Fatalf("same device saw nothing = %v, other %v", rp, other)
	}
	// ... while the same device with the version it saw moves back on purpose.
	rp, other = e.readingPut(t, key, ebookBook, `{"cfi":"epubcfi(/6/2!/4/2/1:0)","progression":0.01,`+
		`"device_id":"web","updated_at":"2026-10-25T11:01:00Z","base_version":5}`)
	if other != nil || rp["version"] != 6.0 || rp["progression"] != 0.01 {
		t.Fatalf("same device on its version = %v, other %v", rp, other)
	}

	// Another user's place is untouched and unseen: kid can't reach the book, and
	// a place of kid's own is kid's alone.
	if resp, _ := e.do(t, "GET", e.at("reading-progress", ebookBook), e.kidTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kid GET outside scope = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "PUT", e.at("reading-progress", ebookBook), e.kidTok,
		`{"cfi":"epubcfi(/6/2!)","progression":0,"device_id":"kid","updated_at":"2026-10-25T11:00:00Z"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kid PUT outside scope = %d, want 403", resp.StatusCode)
	}
	if p, _ := e.cat.GetReadingProgress(context.Background(), e.ann, catalog.Ref{LibraryID: e.lib.ID, Path: ebookBook}); p == nil || p.Version != 6 {
		t.Fatalf("ann's place after kid's refused save = %+v", p)
	}
	if p, _ := e.cat.GetReadingProgress(context.Background(), e.kid, catalog.Ref{LibraryID: e.lib.ID, Path: ebookBook}); p != nil {
		t.Fatalf("kid holds a place: %+v", p)
	}
}

// Denied: a body the rules refuse is 400 and stores nothing; a path with no
// book, or a book with no ebook, is 404.
func TestReadingProgressValidation(t *testing.T) {
	t.Parallel()
	e := newEbookEnv(t)
	good := map[string]any{"cfi": "epubcfi(/6/4!/4/2/1:0)", "progression": 0.5, "device_id": "phone",
		"updated_at": "2026-10-25T10:00:00Z", "base_version": 0}
	body := func(mod func(map[string]any)) string {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		mod(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	for name, mod := range map[string]func(map[string]any){
		"no cfi":            func(m map[string]any) { delete(m, "cfi") },
		"not a cfi":         func(m map[string]any) { m["cfi"] = "/6/4!/4/2" },
		"cfi too long":      func(m map[string]any) { m["cfi"] = "epubcfi(" + strings.Repeat("1", 2048) + ")" },
		"below 0":           func(m map[string]any) { m["progression"] = -0.1 },
		"above 1":           func(m map[string]any) { m["progression"] = 1.5 },
		"progression text":  func(m map[string]any) { m["progression"] = "0.5" },
		"no progression":    func(m map[string]any) { delete(m, "progression") }, // never read as 0
		"null progression":  func(m map[string]any) { m["progression"] = nil },
		"year -1 in UTC":    func(m map[string]any) { m["updated_at"] = "0000-01-01T00:00:00+01:00" },
		"bad updated_at":    func(m map[string]any) { m["updated_at"] = "yesterday" },
		"no updated_at":     func(m map[string]any) { delete(m, "updated_at") },
		"negative base":     func(m map[string]any) { m["base_version"] = -1 },
		"unknown field":     func(m map[string]any) { m["bogus"] = 1 },
		"device id too big": func(m map[string]any) { m["device_id"] = strings.Repeat("d", 201) },
		// Two saves with no device would read as one device's and never diverge.
		"no device id":    func(m map[string]any) { delete(m, "device_id") },
		"empty device id": func(m map[string]any) { m["device_id"] = "" },
	} {
		if resp, out := e.do(t, "PUT", e.at("reading-progress", ebookBook), e.annTok, body(mod)); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %s, want 400", name, resp.StatusCode, out)
		}
	}
	if resp, _ := e.do(t, "PUT", e.at("reading-progress", ebookBook), e.annTok, "not json"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("not json: want 400")
	}
	if p, _ := e.cat.GetReadingProgress(context.Background(), e.ann, catalog.Ref{LibraryID: e.lib.ID, Path: ebookBook}); p != nil {
		t.Fatalf("a refused save stored %+v", p)
	}
	ok := body(func(map[string]any) {})
	for _, p := range []string{plainBook, twoEpubs, "Author", "Author/Missing"} {
		for _, m := range []string{"GET", "PUT"} {
			if resp, out := e.do(t, m, e.at("reading-progress", p), e.annTok, ok); resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s reading-progress %s = %d %s, want 404", m, p, resp.StatusCode, out)
			}
		}
	}
	if resp, _ := e.do(t, "PUT", e.at("reading-progress", ebookBook), "", ok); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthenticated PUT allowed")
	}
	if resp, _ := e.do(t, "PUT", e.at("reading-progress", ebookBook)+"&token="+url.QueryEscape(e.annTok), "", ok); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("?token= PUT allowed")
	}
}

func TestEbooksCapability(t *testing.T) {
	t.Parallel()
	if !serverCapabilities(t, newTestEnv(t))["ebooks"] {
		t.Fatal("ebooks capability is off")
	}
}

// Through the whole handler: the reader document may be framed by this origin
// (X-Frame-Options SAMEORIGIN over the site-wide DENY), the player may not.
func TestReaderDocumentFraming(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":  `<html><head><script>1</script></head><body>app</body></html>`,
		"reader.html": `<html><head><style>b{}</style></head><body><script>2</script></body></html>`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestEnvWith(t, func(c *config.Config) { c.WebDir = dir })
	resp, _ := e.do(t, "GET", "/web/reader.html", "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Fatalf("reader = %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = e.do(t, "GET", "/web/", "", "")
	if resp.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("player = %d %v", resp.StatusCode, resp.Header)
	}
}
