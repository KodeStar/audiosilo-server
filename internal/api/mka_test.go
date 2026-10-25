package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
)

// mkaFixture writes a 4-second Matroska audio file of MP3 with two chapters
// (Opening 0-2 s, Closing 2-4 s) at root/rel, or skips without ffmpeg/ffprobe.
func mkaFixture(t *testing.T, root, rel string) string {
	t.Helper()
	return mkaFixtureOf(t, root, rel, "libmp3lame")
}

// mkaFixtureOf is mkaFixture with the audio encoded by encoder (libmp3lame, aac).
func mkaFixtureOf(t *testing.T, root, rel, encoder string) string {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil || !media.HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg/ffprobe not available")
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(t.TempDir(), "meta.txt")
	chapters := ";FFMETADATA1\ntitle=Matroska Book\nartist=Some Author\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=2000\ntitle=Opening\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=2000\nEND=4000\ntitle=Closing\n"
	if err := os.WriteFile(meta, []byte(chapters), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono", "-i", meta, "-t", "4",
		"-map", "0:a", "-map_metadata", "1", "-map_chapters", "1",
		"-c:a", encoder, "-b:a", "32k", "-y", dst).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg could not write an .mka: %v\n%s", err, out)
	}
	return ffprobe
}

// mkaEnv is a library over two MP3-in-MKA books (one a folder whose first
// part is an .mp3) and an AAC-in-MKA book, scanned with ffprobe and ffmpeg off.
type mkaEnv struct {
	*testEnv
	lib   *catalog.Library
	token string
}

const (
	mkaBook      = "Some Author/Matroska Book"
	mkaFile      = mkaBook + "/book.mka"
	mkaMixed     = "Some Author/Mixed Book"
	mkaMixedPart = mkaMixed + "/02 Main.mka"
	mkaAACBook   = "Other Author/AAC Book"
	mkaAACFile   = mkaAACBook + "/aac.mka"
)

func newMKAEnv(t *testing.T) *mkaEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	ffprobe := mkaFixture(t, root, mkaFile)
	// A folder whose first part plays in browsers and whose second is Matroska: its
	// format is the first part's, but it still isn't direct-playable as stored.
	mkaFixture(t, root, mkaMixedPart)
	mkaFixtureOf(t, root, mkaAACFile, "aac")
	mp3 := filepath.Join(root, "Some Author", "Mixed Book", "01 Intro.mp3")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono",
		"-t", "2", "-c:a", "libmp3lame", "-q:a", "9", "-y", mp3).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not write an .mp3: %v\n%s", err, out)
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := library.NewScanner(e.cat, ffprobe, slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	return &mkaEnv{testEnv: e, lib: lib, token: token}
}

func (m *mkaEnv) url(endpoint, path string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(m.lib.ID, 10) + "/" + endpoint + "?path=" + url.QueryEscape(path)
}

// directPlayable reads direct_playable from /item and /chapters for book,
// failing unless they agree.
func (m *mkaEnv) directPlayable(t *testing.T, book string) bool {
	t.Helper()
	var item struct {
		DirectPlayable *bool `json:"direct_playable"`
	}
	resp, body := m.do(t, "GET", m.url("item", book), m.token, "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &item) != nil || item.DirectPlayable == nil {
		t.Fatalf("item %s = %d %s", book, resp.StatusCode, body)
	}
	var chapters struct {
		DirectPlayable bool `json:"direct_playable"`
	}
	resp, body = m.do(t, "GET", m.url("chapters", book), m.token, "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &chapters) != nil {
		t.Fatalf("chapters %s = %d %s", book, resp.StatusCode, body)
	}
	if *item.DirectPlayable != chapters.DirectPlayable {
		t.Fatalf("%s: item says %v, chapters %v", book, *item.DirectPlayable, chapters.DirectPlayable)
	}
	return chapters.DirectPlayable
}

// cachedFiles counts the remuxed copies in the env's remux cache folder (not
// the .part files of remuxes under way).
func (m *mkaEnv) cachedFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.api.boot.DataDir, "cache", "remux"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mp3") {
			n++
		}
	}
	return n
}

// holdRemuxes holds the env's remuxes until the returned release is called
// (also at cleanup, before the env waits for its remuxes, so a failed test
// never hangs).
func (m *mkaEnv) holdRemuxes(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	m.api.remux.HoldRemuxesForTests(gate)
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// TestMatroskaAudioWithoutFFmpeg (D3a, #161): with ffmpeg off an .mka of MP3 is
// indexed with its container chapters, served as stored (audio/x-matroska) and
// not direct-playable on /item and /chapters, nor is a folder book with an .mka
// part after an .mp3 one: the 2.4.x behaviour.
func TestMatroskaAudioWithoutFFmpeg(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	resp, body := m.do(t, "GET", m.url("item", mkaBook), m.token, "")
	var item struct {
		Format   string `json:"format"`
		Codec    string `json:"codec"`
		Chapters []struct {
			Title string  `json:"title"`
			Start float64 `json:"start"`
		} `json:"chapters"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &item) != nil {
		t.Fatalf("item = %d %s", resp.StatusCode, body)
	}
	if item.Format != "mka" || item.Codec != "mp3" {
		t.Fatalf("item format %q codec %q, want mka and mp3", item.Format, item.Codec)
	}
	if len(item.Chapters) != 2 || item.Chapters[0].Title != "Opening" || item.Chapters[1].Title != "Closing" || item.Chapters[1].Start != 2 {
		t.Fatalf("chapters = %+v, want Opening at 0 and Closing at 2", item.Chapters)
	}
	for _, book := range []string{mkaBook, mkaMixed, mkaAACBook} {
		if m.directPlayable(t, book) {
			t.Errorf("%s direct_playable without ffmpeg, want false", book)
		}
	}
	if resp, _ := m.do(t, "GET", m.url("stream", mkaFile), m.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/x-matroska" {
		t.Fatalf("stream = %d %q, want 200 audio/x-matroska", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if n := m.cachedFiles(t); n != 0 {
		t.Fatalf("%d cached files with ffmpeg off, want none", n)
	}
}

// TestMatroskaMP3IsRemuxed (server#174): with ffmpeg on an .mka of MP3 is served
// at the same URL as its remux to MP3, Range-seekable, and its books (a folder
// with an .mp3 part too) are direct-playable; ?original=1 is the stored bytes,
// ?transcode=1 still transcodes, and AAC in Matroska is served as stored and
// stays not direct-playable.
func TestMatroskaMP3IsRemuxed(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg" // the handlers read this per request

	for book, want := range map[string]bool{mkaBook: true, mkaMixed: true, mkaAACBook: false} {
		if got := m.directPlayable(t, book); got != want {
			t.Errorf("%s direct_playable = %v, want %v", book, got, want)
		}
	}

	resp, body := m.do(t, "GET", m.url("stream", mkaFile), m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mpeg" ||
		resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Last-Modified") == "" ||
		resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("stream = %d %v, want 200 audio/mpeg with Range and a length", resp.StatusCode, resp.Header)
	}
	if body[:3] != "ID3" {
		t.Fatalf("remuxed stream starts %q, want an ID3 tag", body[:3])
	}
	m.api.remux.Wait()
	if n := m.cachedFiles(t); n != 2 {
		t.Fatalf("%d cached files after the remux and the warms of /item, want the two MP3-in-MKA files", n)
	}

	resp, part := m.doHeaders(t, "GET", m.url("stream", mkaFile), m.token, "", map[string]string{"Range": "bytes=0-99"})
	if resp.StatusCode != http.StatusPartialContent || len(part) != 100 || part != body[:100] ||
		resp.Header.Get("Content-Range") != "bytes 0-99/"+strconv.Itoa(len(body)) {
		t.Fatalf("range = %d %q (%d bytes)", resp.StatusCode, resp.Header.Get("Content-Range"), len(part))
	}
	if resp, _ := m.do(t, "HEAD", m.url("stream", mkaFile), m.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("HEAD = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, _ = m.do(t, "GET", m.url("stream", mkaFile)+"&download=1", m.token, "")
	if cd := resp.Header.Get("Content-Disposition"); resp.StatusCode != http.StatusOK || cd != `attachment; filename="book.mp3"` {
		t.Fatalf("download = %d %q, want book.mp3", resp.StatusCode, cd)
	}

	stored, err := os.ReadFile(filepath.Join(m.lib.Root, filepath.FromSlash(mkaFile)))
	if err != nil {
		t.Fatal(err)
	}
	resp, original := m.do(t, "GET", m.url("stream", mkaFile)+"&original=1", m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/x-matroska" || original != string(stored) {
		t.Fatalf("original = %d %q (%d bytes, stored %d)", resp.StatusCode, resp.Header.Get("Content-Type"), len(original), len(stored))
	}
	resp, _ = m.do(t, "GET", m.url("stream", mkaFile)+"&transcode=1", m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("transcode = %d %q %q, want the transcode", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"))
	}

	if resp, _ := m.do(t, "GET", m.url("stream", mkaMixedPart), m.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("mixed part = %d %q, want the remux", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := m.do(t, "GET", m.url("stream", mkaAACFile), m.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/x-matroska" {
		t.Fatalf("aac stream = %d %q, want it as stored", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if n := m.cachedFiles(t); n != 2 {
		t.Fatalf("%d cached files, want the two MP3 remuxes only", n)
	}
}

// TestMatroskaRemuxUnavailableTranscodes: when the remux can't be had (the disk
// guard refuses), an MP3-in-MKA the API called direct_playable is transcoded
// live rather than served as Matroska no browser opens; ?original=1 still
// returns the stored bytes.
func TestMatroskaRemuxUnavailableTranscodes(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	m.api.remux.FakeFreeSpaceForTests(0)

	resp, body := m.do(t, "GET", m.url("stream", mkaFile), m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mpeg" ||
		resp.Header.Get("Accept-Ranges") != "none" || body == "" {
		t.Fatalf("stream = %d %q %q (%d bytes), want the live transcode", resp.StatusCode,
			resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"), len(body))
	}
	if n := m.cachedFiles(t); n != 0 {
		t.Fatalf("%d cached files under the disk guard, want none", n)
	}
	stored, err := os.ReadFile(filepath.Join(m.lib.Root, filepath.FromSlash(mkaFile)))
	if err != nil {
		t.Fatal(err)
	}
	resp, original := m.do(t, "GET", m.url("stream", mkaFile)+"&original=1", m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/x-matroska" || original != string(stored) {
		t.Fatalf("original = %d %q (%d bytes, stored %d)", resp.StatusCode, resp.Header.Get("Content-Type"), len(original), len(stored))
	}
}

// TestMatroskaRemuxCacheRemoved: a remux cache folder removed while the server
// runs is made again, so an MP3-in-MKA is still served as its remux rather than
// as the stored Matroska; a folder named .mka is a 404, as it was.
func TestMatroskaRemuxCacheRemoved(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	if err := os.RemoveAll(filepath.Join(m.api.boot.DataDir, "cache", "remux")); err != nil {
		t.Fatal(err)
	}
	if resp, _ := m.do(t, "GET", m.url("stream", mkaFile), m.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("stream = %d %q %q, want the remux", resp.StatusCode,
			resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"))
	}
	if n := m.cachedFiles(t); n != 1 {
		t.Fatalf("%d cached files, want the remux in the folder made again", n)
	}
	if err := os.Mkdir(filepath.Join(m.lib.Root, filepath.FromSlash(mkaBook), "folder.mka"), 0o700); err != nil {
		t.Fatal(err)
	}
	if resp, body := m.do(t, "GET", m.url("stream", mkaBook+"/folder.mka"), m.token, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("folder stream = %d %s, want 404", resp.StatusCode, body)
	}
}

// TestMatroskaRemuxDenied: the remux runs only after the access checks; a
// caller outside the grant, a traversal and a cast token for another book are
// refused and leave nothing in the cache.
func TestMatroskaRemuxDenied(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	ctx := context.Background()
	share, err := m.cat.CreateShare(ctx, catalog.Share{Name: "One book",
		Paths: []catalog.PathRule{{LibraryID: m.lib.ID, Path: mkaBook}}})
	if err != nil {
		t.Fatal(err)
	}
	reader, readerTok := newUserWithSession(t, m.testEnv, "reader", false)
	if err := m.cat.GrantShare(ctx, reader, share.ID); err != nil {
		t.Fatal(err)
	}
	_, strangerTok := newUserWithSession(t, m.testEnv, "stranger", false)
	mkaFixture(t, filepath.Dir(m.lib.Root), "outside.mka")

	// The admin mints a cast token for the AAC book only.
	body, _ := json.Marshal(map[string]any{"library_id": m.lib.ID, "path": mkaAACBook})
	resp, minted := m.do(t, "POST", "/api/v1/cast/token", m.token, string(body))
	var cast struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(minted), &cast) != nil {
		t.Fatalf("cast mint = %d %s", resp.StatusCode, minted)
	}

	for _, d := range []struct {
		name, url, token string
		want             int
	}{
		{"outside the share", m.url("stream", mkaMixedPart), readerTok, http.StatusForbidden},
		{"no grant", m.url("stream", mkaFile), strangerTok, http.StatusForbidden},
		{"traversal out of the share", m.url("stream", mkaBook+"/../Mixed Book/02 Main.mka"), readerTok, http.StatusForbidden},
		// A leading ".." clamps at the library root (relpath.Clean): the .mka
		// beside the root is never reached, the clamped path holds nothing.
		{"traversal out of the library", m.url("stream", "../outside.mka"), m.token, http.StatusNotFound},
		{"cast token for another book", mediaURL(m.lib.ID, "stream", mkaFile, cast.Token), "", http.StatusUnauthorized},
	} {
		if resp, body := m.do(t, "GET", d.url, d.token, ""); resp.StatusCode != d.want {
			t.Errorf("%s = %d %s, want %d", d.name, resp.StatusCode, body, d.want)
		}
	}
	if n := m.cachedFiles(t); n != 0 {
		t.Fatalf("%d cached files after refused requests, want none", n)
	}
	// Allowed: the reader's own book.
	if resp, _ := m.do(t, "GET", m.url("stream", mkaFile), readerTok, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("reader's book = %d %q, want the remux", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestMatroskaRemuxWarmsOnItem: /item and /chapters start the remuxes of a
// book's MP3-in-MKA files in the background without waiting for them, and skip
// a book whose .mka holds AAC.
func TestMatroskaRemuxWarmsOnItem(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	release := m.holdRemuxes(t)

	// Held remuxes: the answers still come back at once.
	if resp, body := m.do(t, "GET", m.url("item", mkaMixed), m.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("item = %d %s", resp.StatusCode, body)
	}
	if resp, body := m.do(t, "GET", m.url("chapters", mkaBook), m.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("chapters = %d %s", resp.StatusCode, body)
	}
	if n := m.cachedFiles(t); n != 0 {
		t.Fatalf("%d cached files while held, want none yet", n)
	}
	release()
	m.api.remux.Wait()
	if n := m.cachedFiles(t); n != 2 {
		t.Fatalf("%d cached files after the warm, want the two MP3-in-MKA files", n)
	}
	if resp, _ := m.do(t, "GET", m.url("item", mkaAACBook), m.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("aac item = %d", resp.StatusCode)
	}
	m.api.remux.Wait()
	if n := m.cachedFiles(t); n != 2 {
		t.Fatalf("%d cached files after the AAC book's item, want still 2", n)
	}
	// The first stream is a hit: the remux's Range answer, not the transcode.
	if resp, _ := m.do(t, "GET", m.url("stream", mkaMixedPart), m.token, ""); resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("stream after the warm = %d %q, want the cached remux", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}

	// With ffmpeg off nothing is warmed.
	m2 := newMKAEnv(t)
	if resp, _ := m2.do(t, "GET", m2.url("item", mkaBook), m2.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("item without ffmpeg = %d", resp.StatusCode)
	}
	m2.api.remux.Wait()
	if n := m2.cachedFiles(t); n != 0 {
		t.Fatalf("%d cached files with ffmpeg off, want none", n)
	}
}

// TestMatroskaColdRemuxTranscodes: a stream that finds its remux still running
// after the bounded wait is transcoded live (a download still named .mp3), the
// remux goes on, and the next request gets the cached copy.
func TestMatroskaColdRemuxTranscodes(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	m.api.remuxWait = 20 * time.Millisecond
	release := m.holdRemuxes(t)

	resp, body := m.do(t, "GET", m.url("stream", mkaFile), m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mpeg" ||
		resp.Header.Get("Accept-Ranges") != "none" || body == "" {
		t.Fatalf("cold stream = %d %q %q (%d bytes), want the live transcode", resp.StatusCode,
			resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"), len(body))
	}
	resp, _ = m.do(t, "GET", m.url("stream", mkaFile)+"&download=1", m.token, "")
	if cd := resp.Header.Get("Content-Disposition"); resp.Header.Get("Accept-Ranges") != "none" || cd != `attachment; filename="book.mp3"` {
		t.Fatalf("cold download = %q %q, want the transcode named book.mp3", resp.Header.Get("Accept-Ranges"), cd)
	}
	release()
	m.api.remux.Wait()
	resp, _ = m.do(t, "GET", m.url("stream", mkaFile), m.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("stream after the remux = %d %q, want the cached remux", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}
	if n := m.cachedFiles(t); n != 1 {
		t.Fatalf("%d cached files, want the one remux", n)
	}
}

// TestMatroskaRemuxValidators: the remux carries its own validators (a strong
// ETag, the copy's mtime as Last-Modified), so the If-Range and
// If-Modified-Since a 2.4.x client kept from the stored Matroska get the whole
// MP3, never a 304 or MP3 bytes for a Matroska range; its own validators work.
func TestMatroskaRemuxValidators(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	m.api.ffmpeg = "ffmpeg"
	src := filepath.Join(m.lib.Root, filepath.FromSlash(mkaFile))
	stored := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, stored, stored); err != nil {
		t.Fatal(err)
	}
	// What a 2.4.x server sent for the stored .mka.
	resp, _ := m.do(t, "GET", m.url("stream", mkaFile)+"&original=1", m.token, "")
	oldLM := resp.Header.Get("Last-Modified")
	if oldLM != stored.UTC().Format(http.TimeFormat) {
		t.Fatalf("stored Last-Modified = %q", oldLM)
	}

	resp, body := m.do(t, "GET", m.url("stream", mkaFile), m.token, "")
	etag, lm := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(etag, `"remux-`) || lm == "" || lm == oldLM {
		t.Fatalf("remux = %d ETag %q Last-Modified %q (stored %q), want a strong ETag and the copy's mtime", resp.StatusCode, etag, lm, oldLM)
	}

	for _, c := range []struct {
		name    string
		headers map[string]string
		status  int
		length  int
	}{
		{"2.4.x If-Range", map[string]string{"Range": "bytes=0-99", "If-Range": oldLM}, http.StatusOK, len(body)},
		{"2.4.x If-Modified-Since", map[string]string{"If-Modified-Since": oldLM}, http.StatusOK, len(body)},
		{"own If-Range", map[string]string{"Range": "bytes=0-99", "If-Range": etag}, http.StatusPartialContent, 100},
		{"own If-None-Match", map[string]string{"If-None-Match": etag}, http.StatusNotModified, 0},
	} {
		resp, got := m.doHeaders(t, "GET", m.url("stream", mkaFile), m.token, "", c.headers)
		if resp.StatusCode != c.status || len(got) != c.length || (c.length > 0 && got != body[:c.length]) {
			t.Errorf("%s = %d with %d bytes, want %d with %d", c.name, resp.StatusCode, len(got), c.status, c.length)
		}
	}
}
