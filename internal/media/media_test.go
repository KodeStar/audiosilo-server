package media

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAudioContentType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		".m4b":     "audio/mp4",
		".M4B":     "audio/mp4", // case-insensitive
		".aax":     "audio/mp4",
		".mp3":     "audio/mpeg",
		".aac":     "audio/aac",
		".flac":    "audio/flac",
		".ogg":     "audio/ogg",
		".opus":    "audio/opus",
		".wav":     "audio/wav",
		".mka":     "audio/x-matroska",
		".unknown": "",
	}
	for ext, want := range cases {
		if got := audioContentType(ext); got != want {
			t.Fatalf("audioContentType(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestSniffAudioType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"flac", []byte("fLaC\x00\x00\x00\x00"), "audio/flac"},
		{"ogg", []byte("OggS\x00\x00\x00\x00"), "audio/ogg"},
		{"id3 mp3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00\x00\x00\x00"), "audio/mpeg"},
		{"mp4 ftyp", append([]byte{0, 0, 0, 0x18}, []byte("ftypM4A ")...), "audio/mp4"},
		{"riff wave", append([]byte("RIFF\x00\x00\x00\x00"), []byte("WAVE")...), "audio/wav"},
		// ADTS AAC: 0xFF sync with layer bits 00 (0xF1). Its sync word also matches
		// the MPEG mask, so sniffAudioType checks ADTS *before* MP3 - pin that order.
		{"adts aac", []byte{0xFF, 0xF1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, "audio/aac"},
		// MP3 frame sync (0xFF 0xFB) - must not be misread as ADTS.
		{"mp3 frame sync", []byte{0xFF, 0xFB, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, "audio/mpeg"},
		{"matroska ebml", []byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x42, 0x86, 0x81}, "audio/x-matroska"},
		{"unrecognized", []byte("not audio at all"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := writeTempFile(t, "probe.bin", tc.data)
			if got := sniffAudioType(f); got != tc.want {
				t.Fatalf("sniffAudioType = %q, want %q", got, tc.want)
			}
			// It must rewind the file so ServeContent reads from the top.
			if pos, _ := f.Seek(0, io.SeekCurrent); pos != 0 {
				t.Fatalf("file not rewound after sniff: pos = %d", pos)
			}
		})
	}
}

func TestServeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "song.mp3")
	body := append([]byte("ID3"), []byte(strings.Repeat("a", 200))...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("full request sets audio content type and accept-ranges", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), filepath.Dir(path), path, false, time.Minute); err != nil {
			t.Fatal(err)
		}
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); ct != "audio/mpeg" {
			t.Fatalf("Content-Type = %q, want audio/mpeg (sniffed from ID3)", ct)
		}
		if res.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatal("Accept-Ranges should be bytes")
		}
	})

	t.Run("range request yields 206 partial content", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Range", "bytes=0-9")
		if err := ServeFile(rec, req, filepath.Dir(path), path, false, time.Minute); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", rec.Code)
		}
	})

	t.Run("download sets a quoted Content-Disposition", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), filepath.Dir(path), path, true, time.Minute); err != nil {
			t.Fatal(err)
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="song.mp3"` {
			t.Fatalf("Content-Disposition = %q", cd)
		}
	})

	t.Run("missing file is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), dir, filepath.Join(dir, "missing.mp3"), false, time.Minute); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestDirectPlayable(t *testing.T) {
	t.Parallel()
	for _, codec := range []string{"aac", "MP3", "flac", "opus", "vorbis", ""} {
		if !DirectPlayable("m4b", codec) {
			t.Errorf("DirectPlayable(m4b, %q) = false, want true", codec)
		}
	}
	for _, codec := range []string{"ac3", "eac3", "dts", "wmav2"} {
		if DirectPlayable("m4b", codec) {
			t.Errorf("DirectPlayable(m4b, %q) = true, want false", codec)
		}
	}
	// Matroska is never direct, whatever the codec inside or none read (#161).
	for _, codec := range []string{"mp3", "aac", ""} {
		if DirectPlayable("mka", codec) || DirectPlayable("MKA", codec) {
			t.Errorf("DirectPlayable(mka, %q) = true, want false", codec)
		}
	}
}

func TestTranscode(t *testing.T) {
	t.Parallel()
	if !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available; transcoding requires it")
	}
	// A fixture m4b (AAC) is fine input - we only assert it re-encodes to MP3.
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ended, err := Transcode(rec, req, filepath.Dir(src), src, "ffmpeg", 0, false, time.Minute, TranscodeIdle, nil)
	if err != nil || !ended {
		t.Fatalf("Transcode = %v, %v, want a stream that ran to its end", ended, err)
	}

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q, want audio/mpeg", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) == 0 {
		t.Fatal("transcoded output is empty")
	}
	// MP3 frames start with 0xFF or an ID3 header; just confirm real bytes came out.
	if body[0] != 0xFF && string(body[:3]) != "ID3" {
		t.Fatalf("output does not look like MP3 (first bytes %x)", body[:min(4, len(body))])
	}
}

func TestHasFFmpeg(t *testing.T) {
	t.Parallel()
	if HasFFmpeg("") {
		t.Error(`HasFFmpeg("") = true, want false (empty path disables transcoding)`)
	}
	if HasFFmpeg("definitely-not-a-real-binary-xyz") {
		t.Error(`HasFFmpeg("definitely-not-a-real-binary-xyz") = true, want false`)
	}
}

func TestEmbeddedCover(t *testing.T) {
	t.Parallel()
	t.Run("file without embedded art reports ok=false", func(t *testing.T) {
		// None of the committed fixtures carry embedded art; the plain m4b below is
		// the no-art case.
		src := filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b")
		if _, err := os.Stat(src); err != nil {
			t.Skipf("fixture missing: %v", err)
		}
		if data, _, err := EmbeddedCover(t.Context(), src, time.Minute); data != nil || err != nil {
			t.Fatalf("EmbeddedCover of a fixture that has none = %d bytes, %v; want no art, no error", len(data), err)
		}
	})

	t.Run("missing file is an error, no art", func(t *testing.T) {
		if data, _, err := EmbeddedCover(t.Context(), filepath.Join(t.TempDir(), "nope.m4b"), time.Minute); data != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("EmbeddedCover of a missing file = %d bytes, %v; want no art and its error", len(data), err)
		}
	})

	t.Run("file with embedded art reports ok=true with data and mime", func(t *testing.T) {
		// No committed fixture has embedded cover art, so synthesize one with ffmpeg:
		// a 1s tone plus a solid-color JPEG stored as an attached_pic stream.
		if !HasFFmpeg("ffmpeg") {
			t.Skip("ffmpeg not available; cannot synthesize an art-bearing fixture")
		}
		path := filepath.Join(t.TempDir(), "withart.m4b")
		cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-f", "lavfi", "-i", "color=c=red:s=64x64:d=1",
			"-frames:v", "1",
			"-map", "0:a", "-map", "1:v",
			"-c:a", "aac", "-c:v", "mjpeg",
			"-disposition:v", "attached_pic",
			"-f", "mp4", "-y", path,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("ffmpeg could not build an art-bearing fixture: %v\n%s", err, out)
		}

		data, mime, err := EmbeddedCover(t.Context(), path, time.Minute)
		if err != nil {
			t.Fatalf("EmbeddedCover: %v, want the art of a fixture with embedded art", err)
		}
		if len(data) == 0 {
			t.Fatal("EmbeddedCover returned empty data for a fixture with embedded art")
		}
		if mime == "" {
			t.Fatal("EmbeddedCover returned empty mime for a fixture with embedded art")
		}
	})
}

func writeTempFile(t *testing.T, name string, data []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// Embedded art is served as what its bytes are: an image type is kept (allowed),
// and HTML or script bytes, whatever the tag claims, are no art (denied), so a
// crafted audio file can't make the cover endpoint serve a page from this origin.
func TestCoverMIMESniffsTheBytes(t *testing.T) {
	t.Parallel()
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	for name, tc := range map[string]struct {
		data []byte
		mime string
		ok   bool
	}{
		"jpeg":   {jpeg, "image/jpeg", true},
		"png":    {png, "image/png", true},
		"html":   {[]byte("<!doctype html><script>alert(1)</script>"), "", false},
		"script": {[]byte("alert(document.domain)"), "", false},
		"svg":    {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "", false},
	} {
		mime, ok := coverMIME(tc.data)
		if ok != tc.ok || (ok && mime != tc.mime) {
			t.Errorf("%s: coverMIME = %q, %v; want %q, %v", name, mime, ok, tc.mime, tc.ok)
		}
	}
}

func TestTranscodeArgs(t *testing.T) {
	t.Parallel()
	tail := []string{"-vn", "-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3", "pipe:1"}
	for _, tc := range []struct {
		name string
		in   inputSpec
		want []string
	}{
		// The seekable inputs (a descriptor, /dev/fd/3, a re-checked path)
		// seek on the input side, each the only protocol ffmpeg may open.
		{"fd", fdInput, []string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", "fd", "-ss", "8.500", "-i", "fd:"}},
		{"dev fd", inputSpec{proto: "file", url: "/dev/fd/3"}, []string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", "file", "-ss", "8.500", "-i", "/dev/fd/3"}},
		{"path", inputSpec{proto: "file", url: "file:/b/x.m4b"}, []string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", "file", "-ss", "8.500", "-i", "file:/b/x.m4b"}},
		// A pipe can't seek: -ss after -i decodes up to it.
		{"pipe", pipeInput, []string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", "pipe", "-i", "pipe:0", "-ss", "8.500"}},
	} {
		if got := transcodeArgs(tc.in, 8.5); !slices.Equal(got, append(tc.want, tail...)) {
			t.Errorf("%s: transcodeArgs = %q\nwant %q", tc.name, got, append(tc.want, tail...))
		}
	}
	if got := transcodeArgs(fdInput, 0); slices.Contains(got, "-ss") {
		t.Errorf("transcodeArgs from 0 = %q, want no -ss", got)
	}
}

func TestListsInputProtocol(t *testing.T) {
	t.Parallel()
	out := "Supported file protocols:\nInput:\n  async\n  fd\n  file\nOutput:\n  file\n"
	if !listsInputProtocol(out, "fd") {
		t.Error("fd in Input not found")
	}
	if listsInputProtocol("Input:\n  file\n  pipe\nOutput:\n  fd\n", "fd") {
		t.Error("fd only in Output read as an input protocol")
	}
	if listsInputProtocol("", "fd") {
		t.Error("empty output read as listing fd")
	}
}

// transcodeOpened runs TranscodeOpened over src from start and returns the
// output's ffprobe duration.
func transcodeOpened(t *testing.T, ffprobe, src string, start float64) float64 {
	t.Helper()
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A moved offset must not matter: TranscodeOpened starts at the top.
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	TranscodeOpened(rec, httptest.NewRequest(http.MethodGet, "/", nil), f, "ffmpeg", start, false, 0, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("transcode = %d with %d bytes", rec.Code, rec.Body.Len())
	}
	out := filepath.Join(t.TempDir(), "out.mp3")
	if err := os.WriteFile(out, rec.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return ffprobeOf(t, ffprobe, out).duration(t)
}

// nonFaststartM4A is writeNonFaststartM4A's file for a test of TranscodeOpened,
// which seeks only through fd: (it skips for an ffmpeg without it).
func nonFaststartM4A(t *testing.T) string {
	t.Helper()
	needFFmpeg(t)
	if inputFor("ffmpeg") != fdInput {
		t.Skip("this ffmpeg has no fd: input protocol (older than 6.1)")
	}
	return writeNonFaststartM4A(t)
}

// writeNonFaststartM4A writes a 10-second AAC .m4a whose moov box follows its
// mdat (ffmpeg's default without +faststart), which only a seekable input
// demuxes.
func writeNonFaststartM4A(t *testing.T) string {
	t.Helper()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "episode.m4a")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=440:d=10",
		"-c:a", "aac", "-b:a", "64k", "-y", src).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not write an .m4a: %v\n%s", err, out)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat")); moov < mdat {
		t.Fatalf("fixture moov at %d, mdat at %d: want the index at the end", moov, mdat)
	}
	return src
}

// TestTranscodeOpened (server#181): a podcast m4a with its index at the end
// transcodes from a descriptor (pipe:0 could not demux it), and ?t= is an
// input seek on it: from 8 s of 10 the output is the last 2 s.
func TestTranscodeOpened(t *testing.T) {
	t.Parallel()
	ffprobe := needFFmpeg(t)
	src := nonFaststartM4A(t)
	for _, tc := range []struct{ start, want float64 }{{0, 10}, {8, 2}} {
		if d := transcodeOpened(t, ffprobe, src, tc.start); d < tc.want-0.5 || d > tc.want+0.5 {
			t.Errorf("transcoded duration from %v s = %v, want about %v", tc.start, d, tc.want)
		}
	}
}

// TestTranscodeHEAD: a HEAD gets the transcode's headers without an ffmpeg run
// (net/http would discard the whole encode).
func TestTranscodeHEAD(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "book.m4b")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	// No ffmpeg at this path: a run would fail with a 500.
	if ended, err := Transcode(rec, httptest.NewRequest(http.MethodHead, "/", nil), filepath.Dir(src), src, filepath.Join(t.TempDir(), "no-ffmpeg"), 0, false, time.Minute, TranscodeIdle, nil); err != nil || ended {
		t.Fatalf("Transcode = %v, %v, want no error and no stream (a HEAD)", ended, err)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/mpeg" ||
		rec.Header().Get("Accept-Ranges") != "none" || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %v (%d bytes), want the transcode's headers and no run", rec.Code, rec.Header(), rec.Body.Len())
	}
}

// TestSetAttachment: a printable ASCII name is one quoted-string filename (a
// quote and a backslash escaped); any other name is an ASCII stand-in there
// with the name itself as filename*, UTF-8 percent-encoded, so a rune past
// ASCII, a control character or a byte that is no UTF-8 is never saved as
// Quote's escape text ("u3000") nor reaches the header raw.
func TestSetAttachment(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, header, saved string }{
		{"01 - Book.mp3", `attachment; filename="01 - Book.mp3"`, "01 - Book.mp3"},
		{`a "b" \c.mp3`, `attachment; filename="a \"b\" \\c.mp3"`, `a "b" \c.mp3`},
		{"Émile Zola.mp3", `attachment; filename="_mile Zola.mp3"; filename*=UTF-8''%C3%89mile%20Zola.mp3`, "Émile Zola.mp3"},
		{"本\u3000一.mp3", `attachment; filename="___.mp3"; filename*=UTF-8''%E6%9C%AC%E3%80%80%E4%B8%80.mp3`, "本\u3000一.mp3"},
		{"a\r\nb;c.mp3", `attachment; filename="a__b;c.mp3"; filename*=UTF-8''a%0D%0Ab%3Bc.mp3`, "a\r\nb;c.mp3"},
		{"caf\xe9.mp3", `attachment; filename="caf_.mp3"; filename*=UTF-8''caf%EF%BF%BD.mp3`, "caf\uFFFD.mp3"},
	} {
		h := http.Header{}
		SetAttachment(h, c.name)
		got := h.Get("Content-Disposition")
		if got != c.header {
			t.Errorf("%q: Content-Disposition %q, want %q", c.name, got, c.header)
			continue
		}
		disposition, params, err := mime.ParseMediaType(got)
		if err != nil || disposition != "attachment" || params["filename"] != c.saved {
			t.Errorf("%q: parsed as %q %q (%v), want attachment %q", c.name, disposition, params["filename"], err, c.saved)
		}
	}
}

// TestTranscodeDownloadAttachment: a download's transcode is an attachment
// named for the file with the MP3 extension (its HEAD too), its error answers
// are none (a missing file's 404, a failed start's 500), and a stream is none.
func TestTranscodeDownloadAttachment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "01 - \"Book\".m4b")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	noFFmpeg := filepath.Join(dir, "no-ffmpeg")
	for _, c := range []struct {
		name, method, path string
		download           bool
		code               int
		want               string
	}{
		{"HEAD download", http.MethodHead, src, true, http.StatusOK, `attachment; filename="01 - \"Book\".mp3"`},
		{"HEAD stream", http.MethodHead, src, false, http.StatusOK, ""},
		{"missing file", http.MethodGet, filepath.Join(dir, "gone.m4b"), true, http.StatusNotFound, ""},
		{"failed start", http.MethodGet, src, true, http.StatusInternalServerError, ""},
	} {
		rec := httptest.NewRecorder()
		if _, err := Transcode(rec, httptest.NewRequest(c.method, "/", nil), dir, c.path, noFFmpeg, 0, c.download, time.Minute, 0, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if rec.Code != c.code || rec.Header().Get("Content-Disposition") != c.want {
			t.Fatalf("%s = %d with Content-Disposition %q, want %d and %q", c.name, rec.Code, rec.Header().Get("Content-Disposition"), c.code, c.want)
		}
	}
}

// probeCountingFFmpeg is a stand-in ffmpeg that counts its runs and runs
// script for each (sh), and the count so far.
func probeCountingFFmpeg(t *testing.T, script string) (path string, runs func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	path = filepath.Join(dir, "ffmpeg")
	syscall.ForkLock.RLock() // as fakeFFmpeg: no parallel fork inherits the write descriptor
	err := os.WriteFile(path, []byte("#!/bin/sh\necho x >> '"+count+"'\n"+script+"\n"), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return path, func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}
}

// TestInputForRetriesAFailedProbe: a probe that failed stands as no fd: for
// fdProbeRetry, so the transcodes and seeks in between run no probe of their
// own, and is asked again once that has passed.
func TestInputForRetriesAFailedProbe(t *testing.T) {
	t.Parallel()
	failing, probes := probeCountingFFmpeg(t, "exit 1")
	for range 3 {
		if in := inputFor(failing); in != pipeInput {
			t.Fatalf("inputFor(an ffmpeg that fails its probe) = %+v, want pipe:0", in)
		}
	}
	if n := probes(); n != 1 {
		t.Fatalf("%d probes for 3 asks within fdProbeRetry, want 1", n)
	}
	v, _ := fdSupport.Load(failing)
	v.(*fdProbe).at = time.Now().Add(-fdProbeRetry)
	inputFor(failing) // drops the stale answer
	inputFor(failing)
	if n := probes(); n != 2 {
		t.Fatalf("%d probes once fdProbeRetry passed, want 2", n)
	}
}

// TestTranscodeHeadRunsNoFFmpeg: a HEAD is answered with no ffmpeg run, its
// input's protocols probe included, a library file's (Transcode) and an
// opened episode's (TranscodeOpened) alike.
func TestTranscodeHeadRunsNoFFmpeg(t *testing.T) {
	t.Parallel()
	ffmpeg, runs := probeCountingFFmpeg(t, "exit 1")
	dir := t.TempDir()
	src := filepath.Join(dir, "a.m4b")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if _, err := Transcode(rec, httptest.NewRequest(http.MethodHead, "/", nil), dir, src, ffmpeg, 0, false, time.Minute, 0, nil); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d (%v), want 200", rec.Code, err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rec = httptest.NewRecorder()
	if TranscodeOpened(rec, httptest.NewRequest(http.MethodHead, "/", nil), f, ffmpeg, 0, false, 0, nil); rec.Code != http.StatusOK {
		t.Fatalf("HEAD of an opened file = %d, want 200", rec.Code)
	}
	if n := runs(); n != 0 {
		t.Fatalf("a HEAD ran ffmpeg %d times, want none", n)
	}
}

// TestInputForAsksAgainAfterASlowProbe: a probe that ran out of time (a busy
// host) is no answer to keep, so the next ask probes again and a cache run
// gets fd: once the binary answers. Not parallel: it shortens fdProbeTimeout.
func TestInputForAsksAgainAfterASlowProbe(t *testing.T) {
	was := fdProbeTimeout
	t.Cleanup(func() { fdProbeTimeout = was })
	fdProbeTimeout = time.Second
	slow, probes := probeCountingFFmpeg(t, "exec sleep 5")
	inputFor(slow)
	inputFor(slow)
	if n := probes(); n != 2 {
		t.Fatalf("%d probes for 2 asks after a timed-out probe, want 2", n)
	}
}
