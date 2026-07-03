package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAudioContentType(t *testing.T) {
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
		".unknown": "",
	}
	for ext, want := range cases {
		if got := audioContentType(ext); got != want {
			t.Fatalf("audioContentType(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestSniffAudioType(t *testing.T) {
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
	dir := t.TempDir()
	path := filepath.Join(dir, "song.mp3")
	body := append([]byte("ID3"), []byte(strings.Repeat("a", 200))...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("full request sets audio content type and accept-ranges", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), path, false)
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
		ServeFile(rec, req, path, false)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", rec.Code)
		}
	})

	t.Run("download sets a quoted Content-Disposition", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), path, true)
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="song.mp3"` {
			t.Fatalf("Content-Disposition = %q", cd)
		}
	})

	t.Run("missing file is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), filepath.Join(dir, "missing.mp3"), false)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestDirectPlayable(t *testing.T) {
	for _, codec := range []string{"aac", "MP3", "flac", "opus", "vorbis", ""} {
		if !DirectPlayable(codec) {
			t.Errorf("DirectPlayable(%q) = false, want true", codec)
		}
	}
	for _, codec := range []string{"ac3", "eac3", "dts", "wmav2"} {
		if DirectPlayable(codec) {
			t.Errorf("DirectPlayable(%q) = true, want false", codec)
		}
	}
}

func TestTranscode(t *testing.T) {
	if !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available; transcoding requires it")
	}
	// A fixture m4b (AAC) is fine input - we only assert it re-encodes to MP3.
	src := filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	Transcode(rec, req, src, "ffmpeg", 0, nil)

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
	if HasFFmpeg("") {
		t.Error(`HasFFmpeg("") = true, want false (empty path disables transcoding)`)
	}
	if HasFFmpeg("definitely-not-a-real-binary-xyz") {
		t.Error(`HasFFmpeg("definitely-not-a-real-binary-xyz") = true, want false`)
	}
}

func TestEmbeddedCover(t *testing.T) {
	t.Run("file without embedded art reports ok=false", func(t *testing.T) {
		// None of the committed fixtures carry embedded art; the plain m4b below is
		// the no-art case.
		src := filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b")
		if _, err := os.Stat(src); err != nil {
			t.Skipf("fixture missing: %v", err)
		}
		if _, _, ok := EmbeddedCover(src); ok {
			t.Fatal("EmbeddedCover reported art for a fixture that has none")
		}
	})

	t.Run("missing file reports ok=false", func(t *testing.T) {
		if _, _, ok := EmbeddedCover(filepath.Join(t.TempDir(), "nope.m4b")); ok {
			t.Fatal("EmbeddedCover reported art for a missing file")
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

		data, mime, ok := EmbeddedCover(path)
		if !ok {
			t.Fatal("EmbeddedCover ok = false, want true for a fixture with embedded art")
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
