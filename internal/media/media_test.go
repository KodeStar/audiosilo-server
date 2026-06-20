package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	// A fixture m4b (AAC) is fine input — we only assert it re-encodes to MP3.
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
