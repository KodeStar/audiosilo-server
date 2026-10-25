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
	"testing"

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

// cachedFiles counts the files in the env's remux cache folder.
func (m *mkaEnv) cachedFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.api.boot.DataDir, "cache", "remux"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
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
	if n := m.cachedFiles(t); n != 1 {
		t.Fatalf("%d cached files after the remux, want 1", n)
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
