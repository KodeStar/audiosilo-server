package api

import (
	"context"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// metaCoverEnv is a metadata-on env whose Mars rail hands out covers on a local
// cover host: a JPEG (Artemis), a missing image (Project Hail Mary), a page that
// isn't an image (book 4) and a decompression bomb (book 5). The current book is
// "Andy Weir/The Martian" (B00FLIJJSY) in the returned library; the recording's
// own cover is https://c/1.jpg (the mock's works/{id}). fetches counts the cover
// fetches once a test swaps in loopbackFetch through countFetches.
type metaCoverEnv struct {
	*testEnv
	lib     int64
	host    string
	fetches atomic.Int32
}

func newMetaCoverEnv(t *testing.T, enabled bool) *metaCoverEnv {
	t.Helper()
	mux, _ := coverMux(t) // without coverHost's 13 MB PNG (slow to make under -race)
	host := httptest.NewServer(mux)
	t.Cleanup(host.Close)
	entry := func(pos, id, cover string) string {
		return `{"position":"` + pos + `","work":{"id":"` + id + `","title":"` + id + `","authors":[],"series":null,"cover_url":"` + cover + `","added_at":null}}`
	}
	series := `{"id":"mars","name":"Mars","authors":[],"works":[` +
		`{"position":"1","work":{"id":"the-martian","title":"The Martian","authors":[],"series":null,"cover_url":null,"added_at":null}},` +
		entry("2", "artemis", host.URL+"/small.jpg") + "," +
		entry("3", "hail-mary", host.URL+"/missing.jpg") + "," +
		entry("4", "page", host.URL+"/page.html") + "," +
		entry("5", "bomb", host.URL+"/bomb.png") + `]}`
	e := &metaCoverEnv{testEnv: newMetaEnvMock(t, enabled, &mockMetaserve{series: series}), host: host.URL}
	e.lib = seedBook(t, e.testEnv, "Andy Weir/The Martian", "B00FLIJJSY")
	return e
}

// countFetches lets the cover host (loopback) be fetched, counting each fetch.
func (e *metaCoverEnv) countFetches() {
	e.api.fetchCover = func(ctx context.Context, url string, limit int64) ([]byte, error) {
		e.fetches.Add(1)
		return loopbackFetch(ctx, url, limit)
	}
}

// metaCoverPath is the proxy URL for cover on path in lib, with extra query
// (size, token).
func metaCoverPath(lib int64, path, cover, extra string) string {
	p := "/api/v1/libraries/" + strconv.FormatInt(lib, 10) + "/meta/cover?path=" + escape(path) + "&url=" + escape(cover)
	if extra != "" {
		p += "&" + extra
	}
	return p
}

// coverPath is metaCoverPath in the env's library.
func (e *metaCoverEnv) coverPath(path, cover, extra string) string {
	return metaCoverPath(e.lib, path, cover, extra)
}

func TestMetaCoverServesAHandedOutCover(t *testing.T) {
	e := newMetaCoverEnv(t, true)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	e.countFetches()
	artemis := e.coverPath("Andy Weir/The Martian", e.host+"/small.jpg", "size=160")

	// Allowed: the media path, token in the query as an <img> sends it.
	resp, body := e.do(t, "GET", artemis+"&token="+tok, "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("cover = %d %q %.80s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	cfg, err := jpeg.DecodeConfig(strings.NewReader(body))
	if err != nil || cfg.Width > 160 || cfg.Height > 160 {
		t.Fatalf("cover is not a 160 px JPEG thumbnail: %+v %v", cfg, err)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != coverCache {
		t.Fatalf("Cache-Control = %q", cc)
	}
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"community-160-`) {
		t.Fatalf("ETag = %q", etag)
	}

	// Cached: a second ask (another size has its own entry) doesn't fetch again,
	// and a revalidation is a 304.
	if resp, _ := e.do(t, "GET", artemis, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("second cover = %d", resp.StatusCode)
	}
	if n := e.fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times, want 1 (the thumbnail cache)", n)
	}
	if resp, _ := e.doHeaders(t, "GET", artemis, tok, "", map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", resp.StatusCode)
	}

	// An unset size is the default thumbnail.
	if resp, _ := e.do(t, "GET", e.coverPath("Andy Weir/The Martian", e.host+"/small.jpg", ""), tok, ""); resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Header.Get("ETag"), `"community-320-`) {
		t.Fatalf("default size = %d %q", resp.StatusCode, resp.Header.Get("ETag"))
	}
}

func TestMetaCoverRefusesWhatTheEnvelopeDoesNotHandOut(t *testing.T) {
	e := newMetaCoverEnv(t, true)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	e.countFetches()
	const book = "Andy Weir/The Martian"

	// Denied: any URL the book's envelope doesn't carry, however fetchable, is a
	// 404 before any fetch - the cover host's other paths, the server itself,
	// cloud metadata, a different scheme, a near miss of a real one.
	for _, u := range []string{
		e.host + "/other.jpg",
		e.host + "/small.jpg?x=1",
		e.srv.URL + "/api/v1/server",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"https://c/2.jpg",
	} {
		if resp, body := e.do(t, "GET", e.coverPath(book, u, ""), tok, ""); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d %s, want 404", u, resp.StatusCode, body)
		}
	}
	if n := e.fetches.Load(); n != 0 {
		t.Fatalf("refused URLs were fetched %d times", n)
	}

	// No url, a size outside the set: 400.
	if resp, _ := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(e.lib, 10)+"/meta/cover?path="+escape(book), tok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no url = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", e.coverPath(book, e.host+"/small.jpg", "size=100"), tok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("size=100 = %d, want 400", resp.StatusCode)
	}

	// A book with no identifiers has no envelope, so no covers.
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: e.lib, RelPath: "Someone/Untagged",
		Title: "Untagged", Author: "Someone", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do(t, "GET", e.coverPath("Someone/Untagged", e.host+"/small.jpg", ""), tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("untagged book = %d, want 404", resp.StatusCode)
	}
	if n := e.fetches.Load(); n != 0 {
		t.Fatalf("fetched %d times", n)
	}
}

func TestMetaCoverAuthAndScope(t *testing.T) {
	e := newMetaCoverEnv(t, true)
	e.countFetches()
	cover := e.host + "/small.jpg"

	// Denied: no credential, or a bad one in the query.
	if resp, _ := e.do(t, "GET", e.coverPath("Andy Weir/The Martian", cover, ""), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", e.coverPath("Andy Weir/The Martian", cover, "token=nope"), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", resp.StatusCode)
	}

	// A user granted only "Andy Weir" in a library that also holds another book
	// carrying the same rail.
	ctx := context.Background()
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: e.lib, RelPath: "Other Author/Secret",
		Title: "Secret", Author: "Other", ASIN: "B00FLIJJSY", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	kid, _ := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Weir only"})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: e.lib, Path: "Andy Weir"})
	e.cat.GrantShare(ctx, kid.ID, share.ID)
	kidTok, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)

	// Allowed inside the grant; denied (403, never looked up) outside it, even
	// though that book's envelope would hand out the same cover.
	if resp, _ := e.do(t, "GET", e.coverPath("Andy Weir/The Martian", cover, "token="+kidTok), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("in scope = %d, want 200", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", e.coverPath("Other Author/Secret", cover, "token="+kidTok), "", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("out of scope = %d, want 403", resp.StatusCode)
	}
	// A library the user can't reach at all.
	if resp, _ := e.do(t, "GET", metaCoverPath(e.lib+100, "x", cover, "token="+kidTok), "", ""); resp.StatusCode == http.StatusOK {
		t.Fatalf("unknown library = %d", resp.StatusCode)
	}
}

func TestMetaCoverUpstreamFailures(t *testing.T) {
	e := newMetaCoverEnv(t, true)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	const book = "Andy Weir/The Martian"

	// Denied by the address guard: the real fetcher never reaches the server's own
	// network, even for a URL the envelope hands out.
	resp, body := e.do(t, "GET", e.coverPath(book, e.host+"/small.jpg", ""), tok, "")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, codeCoverUnavailable) {
		t.Fatalf("loopback cover = %d %s, want 502 cover_unavailable", resp.StatusCode, body)
	}

	e.countFetches()
	// The image's host fails: 502, not cached (asked again, fetched again).
	for range 2 {
		if resp, body := e.do(t, "GET", e.coverPath(book, e.host+"/missing.jpg", ""), tok, ""); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("missing image = %d %s, want 502", resp.StatusCode, body)
		}
	}
	if n := e.fetches.Load(); n != 2 {
		t.Fatalf("a failed fetch was cached: %d fetches", n)
	}
	// Not an image, and a decompression bomb (too large to decode): 404, cached
	// as none.
	for _, u := range []string{e.host + "/page.html", e.host + "/bomb.png"} {
		if resp, body := e.do(t, "GET", e.coverPath(book, u, ""), tok, ""); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d %s, want 404", u, resp.StatusCode, body)
		}
	}
	// The recording's own cover is handed out too (here an unreachable host).
	if resp, _ := e.do(t, "GET", e.coverPath(book, "https://c/1.jpg", ""), tok, ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("recording cover = %d, want 502 (fetched, host unreachable)", resp.StatusCode)
	}
}

func TestMetaCoverMetaUnavailable(t *testing.T) {
	// The metadata service is down and nothing is cached: no envelope, no cover.
	e := newMetaEnv(t, true, http.StatusInternalServerError)
	libID := seedBook(t, e, "Andy Weir/The Martian", "B00FLIJJSY")
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if resp, body := e.do(t, "GET", metaCoverPath(libID, "Andy Weir/The Martian", "https://c/1.jpg", ""), tok, ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("meta down = %d %s, want 502", resp.StatusCode, body)
	}
}

func TestMetaCoverDisabled(t *testing.T) {
	e := newMetaCoverEnv(t, false)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "GET", e.coverPath("Andy Weir/The Martian", e.host+"/small.jpg", ""), tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled = %d, want 404", resp.StatusCode)
	}
}
