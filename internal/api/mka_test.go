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
		"-c:a", "libmp3lame", "-q:a", "9", "-y", dst).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg could not write an .mka: %v\n%s", err, out)
	}
	return ffprobe
}

// TestMatroskaAudioPlaysThroughTranscode (D3a, #161): an .mka of MP3 is indexed
// with its container chapters, served as audio/x-matroska, and marked not
// direct-playable on /item and /chapters (no browser plays the container, MP3
// inside or not), so the web player asks ?transcode=1, which serves MP3; so is a
// folder book with an .mka part after an .mp3 one.
func TestMatroskaAudioPlaysThroughTranscode(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	ffprobe := mkaFixture(t, root, "Some Author/Matroska Book/book.mka")
	// A folder whose first part plays in browsers and whose second is Matroska: its
	// format is the first part's, but it still isn't direct-playable.
	mkaFixture(t, root, "Some Author/Mixed Book/02 Main.mka")
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
	base := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	file := url.QueryEscape("Some Author/Matroska Book/book.mka")
	book := url.QueryEscape("Some Author/Matroska Book")

	resp, body := e.do(t, "GET", base+"/item?path="+book, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item = %d %s", resp.StatusCode, body)
	}
	var item struct {
		Format         string `json:"format"`
		Codec          string `json:"codec"`
		DirectPlayable *bool  `json:"direct_playable"`
		Chapters       []struct {
			Title string  `json:"title"`
			Start float64 `json:"start"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(body), &item); err != nil {
		t.Fatal(err)
	}
	if item.Format != "mka" || item.DirectPlayable == nil || *item.DirectPlayable {
		t.Fatalf("item format %q direct_playable %v, want mka and false (%s)", item.Format, item.DirectPlayable, body)
	}
	if len(item.Chapters) != 2 || item.Chapters[0].Title != "Opening" || item.Chapters[1].Title != "Closing" || item.Chapters[1].Start != 2 {
		t.Fatalf("chapters = %+v, want Opening at 0 and Closing at 2", item.Chapters)
	}
	var chapters struct {
		DirectPlayable bool `json:"direct_playable"`
	}
	if resp, body := e.do(t, "GET", base+"/chapters?path="+book, token, ""); resp.StatusCode != http.StatusOK ||
		json.Unmarshal([]byte(body), &chapters) != nil || chapters.DirectPlayable {
		t.Fatalf("chapters = %d %s, want direct_playable false", resp.StatusCode, body)
	}

	mixed := url.QueryEscape("Some Author/Mixed Book")
	if resp, body := e.do(t, "GET", base+"/item?path="+mixed, token, ""); resp.StatusCode != http.StatusOK ||
		json.Unmarshal([]byte(body), &item) != nil || item.Format != "mp3" || item.DirectPlayable == nil || *item.DirectPlayable {
		t.Fatalf("mixed item = %d %s, want format mp3 and direct_playable false", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "GET", base+"/chapters?path="+mixed, token, ""); resp.StatusCode != http.StatusOK ||
		json.Unmarshal([]byte(body), &chapters) != nil || chapters.DirectPlayable {
		t.Fatalf("mixed chapters = %d %s, want direct_playable false", resp.StatusCode, body)
	}

	if resp, _ := e.do(t, "GET", base+"/stream?path="+file, token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/x-matroska" {
		t.Fatalf("stream = %d %q, want 200 audio/x-matroska", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	e.api.ffmpeg = "ffmpeg" // the handler reads this per-request
	if resp, _ := e.do(t, "GET", base+"/stream?transcode=1&path="+file, token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("transcoded stream = %d %q, want 200 audio/mpeg", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
