package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// bigJSON is a JSON answer comfortably over gzipMinSize.
var bigJSON = []byte(`{"books":[` + strings.Repeat(`{"title":"A Book","author":"An Author"},`, 60) + `{}]}` + "\n")

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v (%q)", err, b[:min(len(b), 40)])
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hasVaryAE(h http.Header) bool { return hasVary(h, "Accept-Encoding") }

// serveCompress runs h behind compress for one request and returns the recorder.
func serveCompress(h http.HandlerFunc, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	compress(h).ServeHTTP(w, r)
	return w
}

func jsonHandler(status int, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

var acceptGzip = map[string]string{"Accept-Encoding": "gzip, deflate, br"}

// TestCompressJSON: a JSON answer of at least gzipMinSize to a client taking
// gzip is gzipped (status kept, Content-Length dropped, Vary set); a smaller
// one, or one to a client that doesn't take gzip (no header, q=0, identity
// only), goes out as it is, still with Vary.
func TestCompressJSON(t *testing.T) {
	t.Parallel()
	const p = "/api/v1/libraries/1/books"
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		w := serveCompress(jsonHandler(status, bigJSON), "GET", p, acceptGzip)
		if w.Code != status || w.Header().Get("Content-Encoding") != "gzip" || !hasVaryAE(w.Header()) ||
			w.Header().Get("Content-Length") != "" {
			t.Fatalf("status %d: %d %v", status, w.Code, w.Header())
		}
		if got := gunzip(t, w.Body.Bytes()); !bytes.Equal(got, bigJSON) {
			t.Fatalf("decoded %q", got)
		}
		if w.Body.Len() >= len(bigJSON)/2 {
			t.Fatalf("gzip %d bytes of %d", w.Body.Len(), len(bigJSON))
		}
	}

	small := []byte(`{"ok":true}` + "\n")
	w := serveCompress(jsonHandler(http.StatusOK, small), "GET", p, acceptGzip)
	if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), small) || !hasVaryAE(w.Header()) ||
		w.Header().Get("Content-Length") != strconv.Itoa(len(small)) {
		t.Fatalf("small answer: %v %q", w.Header(), w.Body.Bytes())
	}
	// Exactly the threshold is compressed, one byte under is not.
	at := append(bytes.Repeat([]byte(" "), gzipMinSize-3), []byte("{}\n")...)
	if w := serveCompress(jsonHandler(200, at), "GET", p, acceptGzip); w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("%d bytes not compressed", len(at))
	}
	if w := serveCompress(jsonHandler(200, at[1:]), "GET", p, acceptGzip); w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("%d bytes compressed", len(at)-1)
	}

	for _, ae := range []string{"", "gzip;q=0", "identity", "br", "*;q=0"} {
		w := serveCompress(jsonHandler(http.StatusOK, bigJSON), "GET", p, map[string]string{"Accept-Encoding": ae})
		if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), bigJSON) || !hasVaryAE(w.Header()) {
			t.Fatalf("Accept-Encoding %q: %v", ae, w.Header())
		}
	}
	if w := serveCompress(jsonHandler(200, bigJSON), "GET", p, map[string]string{"Accept-Encoding": "*"}); w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("Accept-Encoding * not compressed")
	}
}

// TestCompressManyWrites: an answer written in small pieces, and one written
// with no WriteHeader, comes out whole.
func TestCompressManyWrites(t *testing.T) {
	t.Parallel()
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < len(bigJSON); i += 7 {
			_, _ = w.Write(bigJSON[i:min(i+7, len(bigJSON))])
		}
	}
	w := serveCompress(h, "GET", "/api/v1/search", acceptGzip)
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(gunzip(t, w.Body.Bytes()), bigJSON) {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	// Nothing written: the empty 200, untouched.
	w = serveCompress(func(http.ResponseWriter, *http.Request) {}, "GET", "/api/v1/search", acceptGzip)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("empty: %d %v", w.Code, w.Header())
	}
}

// TestCompressLeavesOtherAnswers: what isn't JSON (audio, an image, HTML), an
// answer already encoded (its Vary not doubled), one with an ETag of its own,
// and the bodiless statuses pass through untouched.
func TestCompressLeavesOtherAnswers(t *testing.T) {
	t.Parallel()
	const p = "/api/v1/libraries/1/item"
	for _, ct := range []string{"audio/mpeg", "image/jpeg", "text/html; charset=utf-8", "application/epub+zip", ""} {
		h := func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			_, _ = w.Write(bigJSON)
		}
		w := serveCompress(h, "GET", p, acceptGzip)
		if w.Header().Get("Content-Encoding") != "" || hasVaryAE(w.Header()) || !bytes.Equal(w.Body.Bytes(), bigJSON) {
			t.Fatalf("%q: %v", ct, w.Header())
		}
	}

	pre := gzipBytes(bigJSON)
	encoded := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		_, _ = w.Write(pre)
	}
	w := serveCompress(encoded, "GET", p, acceptGzip)
	if !bytes.Equal(w.Body.Bytes(), pre) || len(w.Header().Values("Vary")) != 1 {
		t.Fatalf("already encoded: %v", w.Header())
	}

	etag := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		jsonHandler(200, bigJSON)(w, nil)
	}
	if w := serveCompress(etag, "GET", p, acceptGzip); w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), bigJSON) {
		t.Fatalf("with an ETag: %v", w.Header())
	}
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		w := serveCompress(jsonHandler(code, nil), "GET", p, acceptGzip)
		if w.Code != code || w.Header().Get("Content-Encoding") != "" || w.Body.Len() != 0 {
			t.Fatalf("%d: %d %v", code, w.Code, w.Header())
		}
	}
}

// TestCompressExclusions: the request half of the rule. Only a GET (or its
// HEAD) under /api/v1/ is compressed; never a POST or other write (every
// credential is minted by one), a Range request, the media (stream, cover, ebook,
// /meta/cover, a backup's download), /api/v1/auth/* (credentials) or the
// Bridge's routes (the long-poll claim), however large the JSON.
func TestCompressExclusions(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		method, path string
		headers      map[string]string
		want         bool
	}{
		{"GET", "/api/v1/libraries/1/books", nil, true},
		{"GET", "/api/v1/admin/books", nil, true},
		{"GET", "/api/v1/server", nil, true},
		{"GET", "/api/v1/libraries/1/meta", nil, true},
		{"POST", "/api/v1/auth/login", nil, false},
		{"POST", "/api/v1/auth/pair", nil, false},
		{"POST", "/api/v1/auth/exchange", nil, false},
		{"POST", "/api/v1/auth/tokens", nil, false},
		{"GET", "/api/v1/auth/tokens", nil, false},
		{"POST", "/api/v1/cast/token", nil, false},
		{"POST", "/api/v1/me/household/members/2/session", nil, false},
		{"POST", "/api/v1/admin/users/2/authcode", nil, false},
		{"POST", "/api/v1/admin/bridge/pair", nil, false},
		{"POST", "/api/v1/bridge/pair", nil, false},
		{"POST", "/api/v1/demo/session", nil, false},
		{"POST", "/api/v1/admin/covers", nil, false},
		{"PUT", "/api/v1/me/queue", nil, false},
		{"HEAD", "/api/v1/libraries/1/books", nil, true},
		{"HEAD", "/api/v1/libraries/1/stream", nil, false},
		{"HEAD", "/api/v1/libraries/1/ebook", nil, false},
		{"HEAD", "/api/v1/admin/backups/audiosilo-2026.db", nil, false},
		{"HEAD", "/api/v1/auth/tokens", nil, false},
		{"HEAD", bridgeClaimPath, nil, false},
		{"GET", "/api/v1/libraries/1/books", map[string]string{"Range": "bytes=0-9"}, false},
		{"GET", "/api/v1/libraries/1/stream", nil, false},
		{"GET", "/api/v1/libraries/1/cover", nil, false},
		{"GET", "/api/v1/libraries/1/ebook", nil, false},
		{"GET", "/api/v1/libraries/1/meta/cover", nil, false},
		{"GET", "/api/v1/admin/backups/audiosilo-2026.db", nil, false},
		{"GET", "/api/v1/bridge/jobs/next", nil, false},
		{"GET", "/api/v1/bridge/libraries/1/book", nil, false},
		{"GET", "/web/index.html", nil, false},
		{"GET", "/admin/", nil, false},
		{"GET", "/setup", nil, false},
		{"GET", "/healthz", nil, false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := compressible(r); got != c.want {
			t.Errorf("%s %s %v: compressible = %v, want %v", c.method, c.path, c.headers, got, c.want)
		}
		hdr := map[string]string{"Accept-Encoding": "gzip"}
		for k, v := range c.headers {
			hdr[k] = v
		}
		w := serveCompress(jsonHandler(200, bigJSON), c.method, c.path, hdr)
		if gz := w.Header().Get("Content-Encoding") == "gzip"; gz != c.want {
			t.Errorf("%s %s: gzipped = %v, want %v", c.method, c.path, gz, c.want)
		}
	}
}

// compressLibrary indexes n books with longish titles (a page of them is well
// over gzipMinSize) and returns the library and an admin token.
func compressLibrary(t *testing.T, e *testEnv, n int) (*catalog.Library, string) {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		p := fmt.Sprintf("Author %02d/Book %02d.m4b", i, i)
		b := &catalog.Book{LibraryID: lib.ID, RelPath: p, Title: fmt.Sprintf("The Rather Long Title of Book %02d", i),
			Author: fmt.Sprintf("Author %02d", i), Format: "m4b", Duration: 60, Size: 1000,
			AddedAt: fmt.Sprintf("2025-01-%02dT00:00:00Z", i%28+1),
			Files:   []catalog.BookFile{{RelPath: p, Duration: 60, Format: "m4b", Size: 1000}}}
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	return lib, tok
}

// raw requests path through the whole chain with no transparent decoding:
// gzip says whether to send Accept-Encoding: gzip.
func (e *testEnv) raw(t *testing.T, method, path, tok string, gzip bool, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	return e.rawBody(t, method, path, tok, "", gzip, headers)
}

// rawBody is raw with a request body.
func (e *testEnv) rawBody(t *testing.T, method, path, tok, body string, gzip bool, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// TestCompressThroughTheChain: through the server's whole handler (the timeout
// buffer included), the list cache's routes and an uncached one answer gzip to
// a client taking it and identity otherwise, the same JSON either way, both
// with Vary: Accept-Encoding.
func TestCompressThroughTheChain(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	lib, tok := compressLibrary(t, e, 40)
	for _, p := range []string{
		fmt.Sprintf("/api/v1/libraries/%d/books?limit=200", lib.ID), // list cache
		"/api/v1/admin/books?limit=60",                              // list cache (admin)
		fmt.Sprintf("/api/v1/libraries/%d/authors", lib.ID),         // list cache (browse)
		"/api/v1/admin/authors",                                     // not cached
	} {
		for range 2 { // a miss, then (for a cached route) a hit
			plain, body := e.raw(t, "GET", p, tok, false, nil)
			gz, zbody := e.raw(t, "GET", p, tok, true, nil)
			if plain.StatusCode != 200 || gz.StatusCode != 200 {
				t.Fatalf("GET %s: %d / %d", p, plain.StatusCode, gz.StatusCode)
			}
			if plain.Header.Get("Content-Encoding") != "" || gz.Header.Get("Content-Encoding") != "gzip" ||
				!hasVaryAE(plain.Header) || !hasVaryAE(gz.Header) {
				t.Fatalf("GET %s headers: %v / %v", p, plain.Header, gz.Header)
			}
			if len(body) < gzipMinSize || !bytes.Equal(gunzip(t, zbody), body) {
				t.Fatalf("GET %s: gzip decodes to other bytes (%d plain)", p, len(body))
			}
			if gz.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("GET %s Content-Type %q", p, gz.Header.Get("Content-Type"))
			}
		}
	}
}

// TestCompressHeadMatchesGet: a HEAD answers with the headers its GET would
// (server#270): Vary: Accept-Encoding always, Content-Encoding: gzip for a
// client taking it, the same Content-Type and Content-Length, and no body.
func TestCompressHeadMatchesGet(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	lib, tok := compressLibrary(t, e, 40)
	for _, p := range []string{
		fmt.Sprintf("/api/v1/libraries/%d/books?limit=200", lib.ID), // list cache
		"/api/v1/admin/authors",                                     // not cached
	} {
		for _, gz := range []bool{false, true} {
			get, _ := e.raw(t, "GET", p, tok, gz, nil)
			head, body := e.raw(t, "HEAD", p, tok, gz, nil)
			if get.StatusCode != 200 || head.StatusCode != 200 || len(body) != 0 {
				t.Fatalf("%s gzip=%v: GET %d, HEAD %d with %d body bytes", p, gz, get.StatusCode, head.StatusCode, len(body))
			}
			for _, h := range []string{"Vary", "Content-Encoding", "Content-Type", "Content-Length"} {
				if g, hd := get.Header.Values(h), head.Header.Values(h); fmt.Sprint(g) != fmt.Sprint(hd) {
					t.Errorf("%s gzip=%v %s: GET %q, HEAD %q", p, gz, h, g, hd)
				}
			}
			if want := map[bool]string{false: "", true: "gzip"}[gz]; head.Header.Get("Content-Encoding") != want || !hasVaryAE(head.Header) {
				t.Errorf("%s gzip=%v HEAD headers %v", p, gz, head.Header)
			}
		}
	}
}

// TestListCacheKeepsGzip: a cached answer's gzip is stored with it and sent
// as stored on a hit (no compression per hit), and it goes with the answer at
// the next content write.
func TestListCacheKeepsGzip(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	lib, tok := compressLibrary(t, e, 40)
	p := fmt.Sprintf("/api/v1/libraries/%d/books?limit=200", lib.ID)
	_, first := e.raw(t, "GET", p, tok, true, nil)
	entry := func() *listEntry {
		e.api.lists.mu.Lock()
		defer e.api.lists.mu.Unlock()
		if len(e.api.lists.entries) != 1 {
			t.Fatalf("entries = %d", len(e.api.lists.entries))
		}
		for _, el := range e.api.lists.entries {
			return el.Value.(*listEntry)
		}
		return nil
	}
	got := entry()
	if got.gz == nil || !bytes.Equal(got.gz, first) || !bytes.Equal(gunzip(t, got.gz), got.body) {
		t.Fatal("the stored gzip is not the answer sent")
	}
	// A hit sends the stored bytes: swap them for a marker's gzip.
	marker := []byte(`{"cached":"gzip"}` + "\n")
	e.api.lists.mu.Lock()
	got.gz = gzipBytes(marker)
	e.api.lists.mu.Unlock()
	resp, hit := e.raw(t, "GET", p, tok, true, nil)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(gunzip(t, hit), marker) ||
		resp.Header.Get("Content-Length") != strconv.Itoa(len(hit)) || !hasVaryAE(resp.Header) {
		t.Fatalf("hit: %v %q", resp.Header, gunzip(t, hit))
	}
	// An identity client on the same entry gets the stored body.
	if _, plain := e.raw(t, "GET", p, tok, false, nil); !bytes.Equal(plain, got.body) {
		t.Fatal("identity hit is not the stored body")
	}

	// A content write: the gzip goes with the answer.
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: lib.ID, RelPath: "Author 00/Book 00.m4b",
		Title: "Renamed Book", Author: "Author 00", Format: "m4b", Duration: 60, Size: 1000,
		Files: []catalog.BookFile{{RelPath: "Author 00/Book 00.m4b", Duration: 60, Format: "m4b", Size: 1000}}}); err != nil {
		t.Fatal(err)
	}
	_, after := e.raw(t, "GET", p, tok, true, nil)
	if body := gunzip(t, after); !bytes.Contains(body, []byte(`"title":"Renamed Book"`)) {
		t.Fatalf("after the write: %s", body)
	}
	if now := entry(); bytes.Equal(gunzip(t, now.gz), marker) || !bytes.Equal(gunzip(t, now.gz), now.body) {
		t.Fatal("the old gzip outlived the write")
	}
}

// TestCompressLeavesMediaAndCredentials: through the whole chain, a stream
// (whole and by Range), a cover and a sign-in never carry Content-Encoding,
// even to a client taking gzip.
func TestCompressLeavesMediaAndCredentials(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	scanFixture(t, e, lib)
	tok, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	book := url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	resp, whole := e.raw(t, "GET", base+"/stream?path="+book, tok, true, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "" || len(whole) == 0 {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	resp, part := e.raw(t, "GET", base+"/stream?path="+book, tok, true, map[string]string{"Range": "bytes=0-99"})
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Encoding") != "" || !bytes.Equal(part, whole[:100]) {
		t.Fatalf("stream range: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = e.raw(t, "GET", base+"/cover?path="+book, tok, true, nil)
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("cover: %d %v", resp.StatusCode, resp.Header)
	}
	login, b := e.rawBody(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin-password","device_name":"x"}`,
		true, map[string]string{"Content-Type": "application/json"})
	if login.StatusCode != 200 || login.Header.Get("Content-Encoding") != "" || !bytes.Contains(b, []byte(`"token"`)) {
		t.Fatalf("login: %d %v %s", login.StatusCode, login.Header, b)
	}
}

// BenchmarkListCacheHit is a cached 200-book page answered to a gzip client
// (the stored gzip, no compression) against the same page compressed per
// request, for the S5 note: go test -run - -bench ListCacheHit ./internal/api.
func BenchmarkListCacheHit(b *testing.B) {
	body := bytes.Repeat([]byte(`{"title":"The Rather Long Title of Book","author":"Author","path":"Author/Book.m4b","duration":36000},`), 200)
	gz := gzipBytes(body)
	r := httptest.NewRequest("GET", "/api/v1/libraries/1/books", nil)
	r = r.WithContext(context.WithValue(r.Context(), gzipKey, true))
	b.Run("stored-gzip", func(b *testing.B) {
		for b.Loop() {
			writeCachedJSON(httptest.NewRecorder(), r, body, gz)
		}
	})
	b.Run("gzip-per-hit", func(b *testing.B) {
		for b.Loop() {
			_ = gzipBytes(body)
		}
	})
	b.Logf("raw %d bytes, gzip %d bytes", len(body), len(gz))
}
