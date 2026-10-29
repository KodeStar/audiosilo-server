package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Bridge Phase 3c-1: read-along maps on the player's wire (GET /readalong,
// Book.readalong) and the readalong.align job.

// readalongMapJSON is a read-along map made from an ebook with digest sha over
// files (book-relative), as the Bridge writes it.
func readalongMapJSON(t *testing.T, sha string, files []metadata.ReadalongFile) []byte {
	t.Helper()
	var total float64
	for _, f := range files {
		total += f.Duration
	}
	raw, err := json.Marshal(map[string]any{
		"format": metadata.ReadalongFormat, "ebook": map[string]any{"file": "Book.epub", "sha256": sha},
		"audio": map[string]any{"duration": total, "files": files}, "granularity": "sentence",
		"generator":    map[string]any{"name": "audiosilo-bridge", "version": "0.5.0"},
		"generated_at": "2026-11-02T10:11:12Z", "coverage": map[string]any{"text": 0.996, "audio": 0.987},
		"sections": []any{map[string]any{"href": "Text/ch01.xhtml",
			"spans": []any{map[string]any{"cfi": "epubcfi(/6/4!/4/2,/1:0,/1:118)", "start": 31.2, "end": 37.85}}}},
		"audio_gaps": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// bookAudio is the indexed book at path's audio files as a map names them.
func bookAudio(t *testing.T, cat *catalog.Catalog, libID int64, path string) []metadata.ReadalongFile {
	t.Helper()
	b, err := cat.GetBookByPath(context.Background(), libID, path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.BookAudioFiles(b.RelPath, b.IsFolder, b.Duration, b.Files)
}

// setTestDurations gives every indexed book and file a length (the tests run
// without ffprobe), which a scan that finds them unchanged keeps.
func setTestDurations(t *testing.T, e *testEnv) {
	t.Helper()
	for _, q := range []string{`UPDATE books SET duration = 3600.25`, `UPDATE book_files SET duration = 1800.125`} {
		if _, err := e.db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// writeMap writes body as the map at library-relative rel under root.
func writeMap(t *testing.T, root, rel string, body []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readalongEnv is the ebook env with a fresh map beside its folder book
// (ebookBook) and its single-file book (looseBook), scanned again so both are
// indexed and their ebook hashed; maps holds each map's bytes.
type readalongEnv struct {
	*ebookEnv
	maps map[string][]byte
}

// mapPaths are where each book's map is written.
var mapPaths = map[string]string{ebookBook: ebookBook + "/.audiosilo/readalong.json", looseBook: ".audiosilo/Loose.readalong.json"}

func newReadalongEnv(t *testing.T) *readalongEnv {
	t.Helper()
	e := &readalongEnv{ebookEnv: newEbookEnv(t), maps: map[string][]byte{}}
	// The tests run without ffprobe, so the index has no lengths: give every
	// book and file one (a map needs them above 0).
	setTestDurations(t, e.testEnv)
	for book, rel := range mapPaths {
		e.maps[book] = readalongMapJSON(t, sha256Hex(e.epub), bookAudio(t, e.cat, e.lib.ID, book))
		writeMap(t, e.root, rel, e.maps[book])
	}
	// A book with an ebook and no map, and one with a map and no ebook.
	writeMap(t, e.root, plainBook+"/.audiosilo/readalong.json",
		readalongMapJSON(t, sha256Hex(e.epub), bookAudio(t, e.cat, e.lib.ID, plainBook)))
	scanFixture(t, e.testEnv, e.lib)
	return e
}

// itemReadalong is GET /item's readalong for path as raw JSON ("" when absent).
func (e *readalongEnv) itemReadalong(t *testing.T, path string) string {
	t.Helper()
	resp, body := e.do(t, "GET", e.at("item", path), e.annTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item %s = %d %s", path, resp.StatusCode, body)
	}
	var raw map[string]json.RawMessage
	decodeInto(t, body, &raw)
	return string(raw["readalong"])
}

// Allowed: the book's own path with a session or an API key in the header gets
// the map's bytes as JSON, gzipped when accepted (Vary), with a size+mtime
// ETag that revalidates (304) and no shared caching; a single-file book's too.
func TestReadalongServed(t *testing.T) {
	t.Parallel()
	e := newReadalongEnv(t)
	key, err := e.auth.IssueToken(context.Background(), e.ann, auth.KindAPI, "script", 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(mapPaths[ebookBook])))
	if err != nil {
		t.Fatal(err)
	}
	etag := fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
	for _, tok := range []string{e.annTok, key} {
		resp, body := e.do(t, "GET", e.at("readalong", ebookBook), tok, "")
		if resp.StatusCode != http.StatusOK || body != string(e.maps[ebookBook]) {
			t.Fatalf("GET readalong = %d %.80s, want the map", resp.StatusCode, body)
		}
		for name, want := range map[string]string{
			"Content-Type":  "application/json; charset=utf-8",
			"Cache-Control": "private, no-cache",
			"ETag":          etag,
		} {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Accept-Encoding") {
			t.Errorf("Vary = %v", resp.Header.Values("Vary"))
		}
	}
	// gzip, asked for by hand so the client doesn't undo it.
	req, _ := http.NewRequest("GET", e.srv.URL+e.at("readalong", ebookBook), nil)
	req.Header.Set("Authorization", "Bearer "+e.annTok)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	zipped, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || err != nil {
		t.Fatalf("gzip = %d %q %v", resp.StatusCode, resp.Header.Get("Content-Encoding"), err)
	}
	if plain, _ := io.ReadAll(zr); !bytes.Equal(plain, e.maps[ebookBook]) {
		t.Fatalf("gunzipped = %.80s", plain)
	}
	if resp, _ := e.doHeaders(t, "GET", e.at("readalong", ebookBook), e.annTok, "",
		map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", resp.StatusCode)
	}
	if resp, body := e.do(t, "GET", e.at("readalong", looseBook), e.annTok, ""); resp.StatusCode != http.StatusOK ||
		body != string(e.maps[looseBook]) {
		t.Fatalf("single-file book's map = %d %.80s", resp.StatusCode, body)
	}
}

// Denied: never by ?token= (nor without a credential) 401; outside the
// caller's scope 403; a part's path, the map's own path, a book with an ebook
// and no map, a map with no ebook, a map that fails its checks on disk now 404
// no_readalong; a podcast episode 404.
func TestReadalongDenied(t *testing.T) {
	t.Parallel()
	e := newReadalongEnv(t)
	q := e.at("readalong", ebookBook)
	if resp, _ := e.do(t, "GET", q+"&token="+url.QueryEscape(e.annTok), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("?token= = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", q, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", q, e.kidTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outside kid's scope = %d, want 403", resp.StatusCode)
	}
	for _, p := range []string{ebookTrack, mapPaths[ebookBook], goneBook, plainBook, twoEpubs} {
		if resp, body := e.do(t, "GET", e.at("readalong", p), e.annTok, ""); resp.StatusCode != http.StatusNotFound ||
			(p != mapPaths[ebookBook] && !strings.Contains(body, `"code":"no_readalong"`)) {
			t.Errorf("GET readalong %s = %d %.80s, want 404 no_readalong", p, resp.StatusCode, body)
		}
	}
	// The map on disk no longer checks (the index still holds the old head).
	writeMap(t, e.root, mapPaths[looseBook], []byte(`{"format":"audiosilo-readalong/v9"}`))
	if resp, body := e.do(t, "GET", e.at("readalong", looseBook), e.annTok, ""); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(body, `"code":"no_readalong"`) {
		t.Errorf("an invalid map on disk = %d %s, want 404 no_readalong", resp.StatusCode, body)
	}
	if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(mapPaths[ebookBook]))); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "GET", q, e.annTok, ""); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(body, `"code":"no_readalong"`) {
		t.Errorf("a map gone since the scan = %d %s, want 404 no_readalong", resp.StatusCode, body)
	}
}

// Denied: a podcast episode has no read-along.
func TestReadalongPodcastRefused(t *testing.T) {
	t.Parallel()
	b, _, f := podcastEnv(t, false)
	resp, body := b.doHeaders(t, "GET", bookURL(b.lib.ID, "readalong", f.Episodes[0].Path), b.userTok, "", podcastHeaders())
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"no_readalong"`) {
		t.Errorf("an episode = %d %s, want 404 no_readalong", resp.StatusCode, body)
	}
}

// Book.readalong rides on GET /item and the console's book page only, never a
// list row: ready with the map's text coverage and time; absent for a book with
// no map, or a map but no ebook.
func TestBookReadalongField(t *testing.T) {
	t.Parallel()
	e := newReadalongEnv(t)
	ready := `{"state":"ready","coverage":0.996,"generated_at":"2026-11-02T10:11:12Z"}`
	for path, want := range map[string]string{ebookBook: ready, looseBook: ready, goneBook: "", plainBook: ""} {
		if got := e.itemReadalong(t, path); got != want {
			t.Errorf("%s: readalong = %s, want %q", path, got, want)
		}
	}
	resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/books", e.lib.ID), e.annTok, "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `"readalong"`) {
		t.Fatalf("books list = %d, carries readalong: %v", resp.StatusCode, strings.Contains(body, `"readalong"`))
	}
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	_, body = e.do(t, "GET", "/api/v1/admin"+strings.TrimPrefix(e.at("book", looseBook), "/api/v1"), adminTok, "")
	var d struct {
		Book struct {
			Readalong json.RawMessage `json:"readalong"`
		} `json:"book"`
	}
	decodeInto(t, body, &d)
	if string(d.Book.Readalong) != ready {
		t.Errorf("admin book page readalong = %s", d.Book.Readalong)
	}
	_, body = e.do(t, "GET", fmt.Sprintf("/api/v1/admin/books?library_id=%d", e.lib.ID), adminTok, "")
	if strings.Contains(body, `"readalong"`) {
		t.Errorf("admin list rows carry readalong")
	}
}

// A map goes stale when the ebook changes (once a scan records it: 409
// readalong_stale, Book.readalong stale/ebook_changed) or when the book's audio
// no longer matches what the map was made from (read from the file each
// request: audio_changed); a fresh map made again is ready.
func TestReadalongStale(t *testing.T) {
	t.Parallel()
	e := newReadalongEnv(t)
	// Audio: a map whose file length is a second off.
	files := bookAudio(t, e.cat, e.lib.ID, looseBook)
	files[0].Duration++
	writeMap(t, e.root, mapPaths[looseBook], readalongMapJSON(t, sha256Hex(e.epub), files))
	if resp, body := e.do(t, "GET", e.at("readalong", looseBook), e.annTok, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"readalong_stale"`) || !strings.Contains(body, `"reason":"audio_changed"`) {
		t.Errorf("audio changed = %d %s, want 409 readalong_stale", resp.StatusCode, body)
	}
	if _, err := e.api.scanner.RefreshAttachments(context.Background(), *e.lib, looseBook); err != nil {
		t.Fatal(err)
	}
	if got := e.itemReadalong(t, looseBook); got != `{"state":"stale","reason":"audio_changed"}` {
		t.Errorf("item after the audio changed = %s", got)
	}

	// Ebook: one byte appended, then a scan.
	edited := append(append([]byte{}, e.epub...), 0)
	if err := os.WriteFile(filepath.Join(e.root, filepath.FromSlash(ebookFile)), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e.testEnv, e.lib)
	if resp, body := e.do(t, "GET", e.at("readalong", ebookBook), e.annTok, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"reason":"ebook_changed"`) {
		t.Errorf("ebook changed = %d %s, want 409 readalong_stale", resp.StatusCode, body)
	}
	if got := e.itemReadalong(t, ebookBook); got != `{"state":"stale","reason":"ebook_changed"}` {
		t.Errorf("item after the ebook changed = %s", got)
	}
	// Made again for the new ebook: ready.
	writeMap(t, e.root, mapPaths[ebookBook], readalongMapJSON(t, sha256Hex(edited), bookAudio(t, e.cat, e.lib.ID, ebookBook)))
	if resp, _ := e.do(t, "GET", e.at("readalong", ebookBook), e.annTok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("a map made again = %d, want 200", resp.StatusCode)
	}
}

// A digest missing from the index (never hashed: a hash that failed, say) is
// made on demand, by /item and /readalong alike, and kept.
func TestReadalongHashesOnDemand(t *testing.T) {
	t.Parallel()
	e := newReadalongEnv(t)
	if _, err := e.db.ExecContext(context.Background(), `UPDATE books SET ebook_sha256 = '', ebook_hashed_sig = ''`); err != nil {
		t.Fatal(err)
	}
	if got := e.itemReadalong(t, ebookBook); !strings.Contains(got, `"state":"ready"`) {
		t.Fatalf("item with no digest = %s", got)
	}
	idx, err := e.cat.BookReadalongAt(context.Background(), e.lib.ID, ebookBook)
	if err != nil || !idx.DigestCurrent || idx.EbookSHA256 != sha256Hex(e.epub) {
		t.Fatalf("after the item: %+v, %v; want the digest kept", idx, err)
	}
	if resp, _ := e.do(t, "GET", e.at("readalong", looseBook), e.annTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("readalong with no digest = %d", resp.StatusCode)
	}
}

// The readalong capability is on.
func TestReadalongCapability(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"readalong":true`) {
		t.Errorf("capabilities = %s", body)
	}
}
