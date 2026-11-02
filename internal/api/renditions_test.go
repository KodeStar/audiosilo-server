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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
)

// renditionEnv is a library over a two-part MP3 book, a one-file M4B book in
// its folder and a loose M4B at the root (a book with no book_files rows),
// scanned, with ffmpeg on.
type renditionEnv struct {
	*testEnv
	lib   *catalog.Library
	token string
}

const (
	rendPartsBook = "Some Author/Parts Book"
	rendPart1     = rendPartsBook + "/01 One.mp3"
	rendPart2     = rendPartsBook + "/02 Two.mp3"
	rendM4BBook   = "Other Author/Single Book/book.m4b"
	rendLooseBook = "Loose Book.m4b"
)

// writeTone writes seconds of a tone at root/rel encoded by enc, or skips
// without ffmpeg/ffprobe; it returns ffprobe's path.
func writeTone(t *testing.T, root, rel string, seconds int, enc ...string) string {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil || !media.HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg/ffprobe not available")
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=440:r=22050",
		"-t", strconv.Itoa(seconds)}, enc...)
	if out, err := exec.Command("ffmpeg", append(args, "-y", dst)...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not write %s: %v\n%s", rel, err, out)
	}
	return ffprobe
}

func newRenditionEnv(t *testing.T) *renditionEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	mp3 := []string{"-c:a", "libmp3lame", "-b:a", "64k"}
	ffprobe := writeTone(t, root, rendPart1, 3, mp3...)
	writeTone(t, root, rendPart2, 2, mp3...)
	writeTone(t, root, rendM4BBook, 4, "-c:a", "aac", "-b:a", "64k")
	writeTone(t, root, rendLooseBook, 2, "-c:a", "aac", "-b:a", "64k")
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.NewScanner(e.cat, ffprobe, slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	e.api.ffmpeg = "ffmpeg"
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	return &renditionEnv{testEnv: e, lib: lib, token: token}
}

func (r *renditionEnv) streamURL(path, extra string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(r.lib.ID, 10) + "/stream?path=" + url.QueryEscape(path) + extra
}

func (r *renditionEnv) renditionsURL(path, extra string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(r.lib.ID, 10) + "/renditions?path=" + url.QueryEscape(path) + extra
}

// cached counts the watch renditions in the env's cache folder.
func (r *renditionEnv) cached(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.api.boot.DataDir, "cache", "renditions", "watch"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".m4a") {
			n++
		}
	}
	return n
}

type renditionsAnswer struct {
	Kind  string `json:"kind"`
	Files []struct {
		Path  string `json:"path"`
		State string `json:"state"`
		Bytes int64  `json:"bytes"`
		// RetryAfter is absent (0) but on a remembered refusal.
		RetryAfter int64 `json:"retry_after_seconds"`
	} `json:"files"`
}

// TestRenditionsCapability: GET /server says renditions true with the kinds
// while ffmpeg and the cache are there, false and [] otherwise, to anyone.
func TestRenditionsCapability(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	read := func() (bool, []string) {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/server", "", "")
		var info struct {
			Capabilities map[string]bool `json:"capabilities"`
			Renditions   []string        `json:"renditions"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil || info.Renditions == nil {
			t.Fatalf("server = %d %s", resp.StatusCode, body)
		}
		return info.Capabilities["renditions"], info.Renditions
	}
	if on, kinds := read(); on || len(kinds) != 0 {
		t.Fatalf("without ffmpeg: %v %v, want false and []", on, kinds)
	}
	e.api.ffmpeg = "ffmpeg"
	if on, kinds := read(); !on || !slices.Equal(kinds, []string{"watch"}) {
		t.Fatalf("with ffmpeg: %v %v, want true and [watch]", on, kinds)
	}
	e.api.renditions = nil // no data dir
	if on, kinds := read(); on || len(kinds) != 0 {
		t.Fatalf("without the cache: %v %v, want false and []", on, kinds)
	}
}

// TestRenditionStream: ?rendition=watch serves the file's AAC copy as
// audio/mp4 with a strong ETag, Range (206), If-Range resuming with the ETag
// and a stale one getting the whole copy, ?download=1 named <stem>.m4a; it
// marks no transcode, and ?token= works as for any stream.
func TestRenditionStream(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	resp, body := r.do(t, "GET", r.streamURL(rendPart1, "&rendition=watch&download=1"), r.token, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mp4" ||
		resp.Header.Get("Accept-Ranges") != "bytes" || len(body) < 1000 {
		t.Fatalf("rendition = %d %q %q %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"),
			resp.Header.Get("Accept-Ranges"), len(body))
	}
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"rw1-`) || !strings.HasSuffix(etag, "-"+strconv.Itoa(len(body))+`"`) {
		t.Fatalf("etag = %s, want rw1-<key>-<size>", etag)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="01 One.m4a"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if body[4:8] != "ftyp" {
		t.Fatalf("rendition starts %q, want an MP4", body[:8])
	}

	resp, part := r.doHeaders(t, "GET", r.streamURL(rendPart1, "&rendition=watch"), r.token, "",
		map[string]string{"Range": "bytes=1000-", "If-Range": etag})
	if resp.StatusCode != http.StatusPartialContent || part != body[1000:] || resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("resume = %d, %d bytes, want 206 and the rest", resp.StatusCode, len(part))
	}
	resp, whole := r.doHeaders(t, "GET", r.streamURL(rendPart1, "&rendition=watch"), r.token, "",
		map[string]string{"Range": "bytes=1000-", "If-Range": `"rw1-stale-1"`})
	if resp.StatusCode != http.StatusOK || whole != body {
		t.Fatalf("stale If-Range = %d, %d bytes, want the whole copy", resp.StatusCode, len(whole))
	}
	// The stored file is still the plain stream's answer.
	if resp, _ := r.do(t, "GET", r.streamURL(rendPart1, ""), r.token, ""); resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("plain stream = %q, want the stored MP3", resp.Header.Get("Content-Type"))
	}
	// ?token= media auth.
	if resp, _ := r.do(t, "GET", mediaURL(r.lib.ID, "stream", rendPart1, r.token)+"&rendition=watch", "", ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("ETag") != etag {
		t.Fatalf("?token= rendition = %d %q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	// A download is not a listen: no transcode marked for the device.
	resp, devices := r.do(t, "GET", "/api/v1/me/devices", r.token, "")
	var list struct {
		Devices []struct {
			ID      int64 `json:"id"`
			Current bool  `json:"current"`
		} `json:"devices"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(devices), &list) != nil {
		t.Fatalf("devices = %d %s", resp.StatusCode, devices)
	}
	for _, d := range list.Devices {
		if d.Current && r.api.streams.Transcoding(d.ID, r.lib.ID, rendPartsBook, time.Now()) {
			t.Fatal("a rendition marked the device's session transcoded")
		}
	}
	if n := r.cached(t); n != 1 {
		t.Fatalf("%d renditions cached, want 1", n)
	}
}

// TestRenditionPending: a cold encode past the wait answers 503
// rendition_pending with Retry-After, the encode goes on, and the next request
// gets the copy.
func TestRenditionPending(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	r.api.renditionWait = 20 * time.Millisecond
	release := holdRuns(t, r.api.renditions)
	resp, body := r.do(t, "GET", r.streamURL(rendM4BBook, "&rendition=watch"), r.token, "")
	if resp.StatusCode != http.StatusServiceUnavailable || codeOf(body) != "rendition_pending" ||
		resp.Header.Get("Retry-After") != "15" {
		t.Fatalf("cold rendition = %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	release()
	r.api.renditions.Wait()
	if resp, _ := r.do(t, "GET", r.streamURL(rendM4BBook, "&rendition=watch"), r.token, ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("after the encode = %d %q, want the copy", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestRenditionError: an encode that can't be had for a reason not the file's
// (here the disk guard) answers 503 rendition_error with Retry-After 60 and
// caches nothing, and is not remembered: with room again the copy is made.
func TestRenditionError(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	r.api.renditions.FakeFreeSpaceForTests(0)
	resp, body := r.do(t, "GET", r.streamURL(rendPart1, "&rendition=watch"), r.token, "")
	if resp.StatusCode != http.StatusServiceUnavailable || codeOf(body) != "rendition_error" ||
		resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("low disk = %d %q %s, want 503 rendition_error after 60", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if n := r.cached(t); n != 0 {
		t.Fatalf("%d renditions cached on a low disk, want none", n)
	}
	r.api.renditions.FakeFreeSpaceForTests(1 << 50)
	if resp, body := r.do(t, "GET", r.streamURL(rendPart1, "&rendition=watch"), r.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("with room again = %d %s, want the copy", resp.StatusCode, body)
	}
}

// TestRenditionRefusals: no ffmpeg is 404 rendition_unavailable on all three
// routes; an unknown kind and a podcast library are 400 bad_rendition; a file
// ffmpeg can't decode is 422 rendition_failed (remembered, so listed failed);
// a non-audio path and a missing file are 404.
func TestRenditionRefusals(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	ctx := context.Background()
	garbage := "Some Author/Parts Book/03 Broken.mp3"
	if err := os.WriteFile(filepath.Join(r.lib.Root, filepath.FromSlash(garbage)), []byte("not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.lib.Root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pods, err := r.cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	podStream := "/api/v1/libraries/" + strconv.FormatInt(pods.ID, 10) + "/stream?path=show/ep.mp3&rendition=watch"
	podPrepare := "/api/v1/libraries/" + strconv.FormatInt(pods.ID, 10) + "/renditions?path=show"

	for _, d := range []struct {
		name, method, url, body string
		status                  int
		code                    string
	}{
		{"unknown kind", "GET", r.streamURL(rendPart1, "&rendition=phone"), "", 400, "bad_rendition"},
		{"empty kind", "GET", r.streamURL(rendPart1, "&rendition="), "", 400, "bad_rendition"},
		{"prepare unknown kind", "POST", r.renditionsURL(rendPartsBook, ""), `{"kind":"tv"}`, 400, "bad_rendition"},
		{"prepare no kind", "POST", r.renditionsURL(rendPartsBook, ""), "", 400, "bad_rendition"},
		{"status no kind", "GET", r.renditionsURL(rendPartsBook, ""), "", 400, "bad_rendition"},
		{"podcast stream", "GET", podStream, "", 400, "bad_rendition"},
		{"podcast prepare", "POST", podPrepare, `{"kind":"watch"}`, 400, "bad_rendition"},
		{"undecodable file", "GET", r.streamURL(garbage, "&rendition=watch"), "", 422, "rendition_failed"},
		{"not audio", "GET", r.streamURL("notes.txt", "&rendition=watch"), "", 404, ""},
		{"no file", "GET", r.streamURL("Some Author/gone.mp3", "&rendition=watch"), "", 404, ""},
		{"no book", "GET", r.renditionsURL("Nobody/Nothing", "&kind=watch"), "", 404, ""},
		{"bad body", "POST", r.renditionsURL(rendPartsBook, ""), `{"kind":`, 400, ""},
	} {
		// A player that sees podcast libraries (auth.SeesPodcasts; a watch never does).
		resp, body := r.doHeaders(t, d.method, d.url, r.token, d.body, map[string]string{"X-AudioSilo-Client": "AudioSilo/dev (ios)"})
		if resp.StatusCode != d.status || (d.code != "" && codeOf(body) != d.code) {
			t.Errorf("%s = %d %s, want %d %s", d.name, resp.StatusCode, body, d.status, d.code)
		}
	}
	if resp, body := r.do(t, "GET", r.streamURL(garbage, "&rendition=watch"), r.token, ""); resp.StatusCode != 422 {
		t.Fatalf("undecodable again = %d %s, want the refusal remembered", resp.StatusCode, body)
	}

	r.api.ffmpeg = ""
	for _, d := range []struct{ method, url, body string }{
		{"GET", r.streamURL(rendPart1, "&rendition=watch"), ""},
		{"POST", r.renditionsURL(rendPartsBook, ""), `{"kind":"watch"}`},
		{"GET", r.renditionsURL(rendPartsBook, "&kind=watch"), ""},
	} {
		if resp, body := r.do(t, d.method, d.url, r.token, d.body); resp.StatusCode != 404 || codeOf(body) != "rendition_unavailable" {
			t.Errorf("%s %s without ffmpeg = %d %s, want 404 rendition_unavailable", d.method, d.url, resp.StatusCode, body)
		}
	}
	if n := r.cached(t); n != 0 {
		t.Fatalf("%d renditions cached, want none", n)
	}
}

// TestRenditionRefusalRetryHint (server#289): a remembered refusal says when it
// is tried again: the 422 carries Retry-After (the refusal's hour, in seconds)
// and the renditions answer gives the failed file retry_after_seconds; a file not
// refused (missing, ready) has none, and a failed file that isn't there has
// none either (nothing to retry).
func TestRenditionRefusalRetryHint(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	// The book's second part, indexed, is now bytes ffmpeg can't decode.
	if err := os.WriteFile(filepath.Join(r.lib.Root, filepath.FromSlash(rendPart2)), []byte("not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	hour := int64(time.Hour / time.Second)
	for range 2 { // the refusal made, then remembered
		resp, body := r.do(t, "GET", r.streamURL(rendPart2, "&rendition=watch"), r.token, "")
		secs, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		if resp.StatusCode != 422 || codeOf(body) != "rendition_failed" || err != nil || secs < hour-60 || secs > hour {
			t.Fatalf("refused = %d %q %s, want 422 rendition_failed after about an hour", resp.StatusCode,
				resp.Header.Get("Retry-After"), body)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		extra, body, want := "&kind=watch", "", http.StatusOK
		if method == "POST" {
			extra, body, want = "", `{"kind":"watch"}`, http.StatusAccepted
		}
		resp, out := r.do(t, method, r.renditionsURL(rendPartsBook, extra), r.token, body)
		var a renditionsAnswer
		if resp.StatusCode != want || json.Unmarshal([]byte(out), &a) != nil || len(a.Files) != 2 {
			t.Fatalf("%s renditions = %d %s", method, resp.StatusCode, out)
		}
		if f := a.Files[0]; f.State == "failed" || f.RetryAfter != 0 {
			t.Fatalf("%s: first part %+v, want no retry hint", method, f)
		}
		if f := a.Files[1]; f.State != "failed" || f.RetryAfter < hour-60 || f.RetryAfter > hour {
			t.Fatalf("%s: second part %+v, want failed with retry_after_seconds about an hour", method, f)
		}
		if strings.Count(out, `"retry_after_seconds"`) != 1 {
			t.Fatalf("%s: retry_after_seconds on other files: %s", method, out)
		}
	}
	r.api.renditions.Wait()
	// A failed file that isn't there: failed, nothing to retry.
	if err := os.Remove(filepath.Join(r.lib.Root, filepath.FromSlash(rendPart2))); err != nil {
		t.Fatal(err)
	}
	resp, out := r.do(t, "GET", r.renditionsURL(rendPartsBook, "&kind=watch"), r.token, "")
	var a renditionsAnswer
	if resp.StatusCode != 200 || json.Unmarshal([]byte(out), &a) != nil || len(a.Files) != 2 ||
		a.Files[1].State != "failed" || strings.Contains(out, `"retry_after_seconds"`) {
		t.Fatalf("a missing file = %d %s, want failed with no retry_after_seconds", resp.StatusCode, out)
	}
}

// TestRenditionsDenied: the rendition routes run only after the access checks:
// a caller with no grant or outside their share, and a traversal, are refused
// and start no encode; the reader's own book is allowed.
func TestRenditionsDenied(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	ctx := context.Background()
	share, err := r.cat.CreateShare(ctx, catalog.Share{Name: "One book",
		Paths: []catalog.PathRule{{LibraryID: r.lib.ID, Path: rendPartsBook}}})
	if err != nil {
		t.Fatal(err)
	}
	reader, readerTok := newUserWithSession(t, r.testEnv, "reader", false)
	if err := r.cat.GrantShare(ctx, reader, share.ID); err != nil {
		t.Fatal(err)
	}
	_, strangerTok := newUserWithSession(t, r.testEnv, "stranger", false)
	for _, d := range []struct {
		name, method, url, token, body string
		want                           int
	}{
		{"stream outside the share", "GET", r.streamURL(rendM4BBook, "&rendition=watch"), readerTok, "", 403},
		{"stream no grant", "GET", r.streamURL(rendPart1, "&rendition=watch"), strangerTok, "", 403},
		{"stream traversal", "GET", r.streamURL(rendPartsBook+"/../../Other Author/Single Book/book.m4b", "&rendition=watch"), readerTok, "", 403},
		{"prepare outside the share", "POST", r.renditionsURL("Other Author/Single Book", ""), readerTok, `{"kind":"watch"}`, 403},
		{"prepare no grant", "POST", r.renditionsURL(rendPartsBook, ""), strangerTok, `{"kind":"watch"}`, 403},
		{"status no grant", "GET", r.renditionsURL(rendPartsBook, "&kind=watch"), strangerTok, "", 403},
		{"no token", "POST", r.renditionsURL(rendPartsBook, ""), "", `{"kind":"watch"}`, 401},
	} {
		if resp, body := r.do(t, d.method, d.url, d.token, d.body); resp.StatusCode != d.want {
			t.Errorf("%s = %d %s, want %d", d.name, resp.StatusCode, body, d.want)
		}
	}
	r.api.renditions.Wait()
	if n := r.cached(t); n != 0 {
		t.Fatalf("%d renditions cached after refused requests, want none", n)
	}
	if resp, body := r.do(t, "GET", r.streamURL(rendPart2, "&rendition=watch"), readerTok, ""); resp.StatusCode != 200 {
		t.Fatalf("reader's own book = %d %s, want the rendition", resp.StatusCode, body)
	}
	if resp, body := r.do(t, "POST", r.renditionsURL(rendPartsBook, ""), readerTok, `{"kind":"watch"}`); resp.StatusCode != 202 {
		t.Fatalf("reader's prepare = %d %s, want 202", resp.StatusCode, body)
	}
}

// TestRenditionsPrepare: GET queues nothing (missing); POST queues every file
// of the book in play order (202, pending) and GET then reports each ready with
// its bytes, which the stream's copy matches; a one-file book is one entry, a
// loose file at the root (no book_files rows) too.
func TestRenditionsPrepare(t *testing.T) {
	t.Parallel()
	r := newRenditionEnv(t)
	read := func(method, path, extra, body string, want int) renditionsAnswer {
		t.Helper()
		resp, out := r.do(t, method, r.renditionsURL(path, extra), r.token, body)
		var a renditionsAnswer
		if resp.StatusCode != want || json.Unmarshal([]byte(out), &a) != nil || a.Kind != "watch" {
			t.Fatalf("%s renditions %s = %d %s", method, path, resp.StatusCode, out)
		}
		return a
	}
	states := func(a renditionsAnswer) []string {
		var out []string
		for _, f := range a.Files {
			out = append(out, f.Path+"="+f.State)
		}
		return out
	}
	if got := states(read("GET", rendPartsBook, "&kind=watch", "", 200)); !slices.Equal(got, []string{rendPart1 + "=missing", rendPart2 + "=missing"}) {
		t.Fatalf("status before = %v", got)
	}
	if n := r.cached(t); n != 0 {
		t.Fatalf("GET queued %d encodes, want none", n)
	}
	release := holdRuns(t, r.api.renditions)
	got := read("POST", rendPartsBook, "", `{"kind":"watch"}`, 202)
	if s := states(got); !slices.Equal(s, []string{rendPart1 + "=pending", rendPart2 + "=pending"}) {
		t.Fatalf("prepare = %v, want both pending in play order", s)
	}
	release()
	r.api.renditions.Wait()
	after := read("GET", rendPartsBook, "&kind=watch", "", 200)
	for _, f := range after.Files {
		if f.State != "ready" || f.Bytes <= 0 {
			t.Fatalf("after the encodes: %+v, want ready with bytes", after.Files)
		}
		resp, body := r.do(t, "GET", r.streamURL(f.Path, "&rendition=watch"), r.token, "")
		if resp.StatusCode != 200 || int64(len(body)) != f.Bytes {
			t.Fatalf("stream %s = %d %d bytes, want %d", f.Path, resp.StatusCode, len(body), f.Bytes)
		}
	}
	// A part path names its book, and a ready book's prepare starts nothing.
	if s := states(read("POST", rendPart2, "", `{"kind":"watch"}`, 202)); !slices.Equal(s, []string{rendPart1 + "=ready", rendPart2 + "=ready"}) {
		t.Fatalf("prepare of a ready book by a part = %v", s)
	}
	if s := states(read("GET", rendM4BBook, "&kind=watch", "", 200)); !slices.Equal(s, []string{rendM4BBook + "=missing"}) {
		t.Fatalf("single-file book = %v", s)
	}
	if s := states(read("GET", rendLooseBook, "&kind=watch", "", 200)); !slices.Equal(s, []string{rendLooseBook + "=missing"}) {
		t.Fatalf("loose book = %v, want its own file", s)
	}
	if s := states(read("POST", rendLooseBook, "", `{"kind":"watch"}`, 202)); !slices.Equal(s, []string{rendLooseBook + "=pending"}) {
		t.Fatalf("loose book's prepare = %v, want its own file queued", s)
	}
}
