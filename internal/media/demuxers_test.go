package media

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// withoutAllowList is args with the demuxer allow-list taken out: ffmpeg left
// to guess among every demuxer it has, as it was before the list.
func withoutAllowList(t *testing.T, args []string) []string {
	t.Helper()
	at := slices.Index(args, "-format_whitelist")
	if at < 0 {
		t.Fatalf("%q has no -format_whitelist", args)
	}
	return slices.Delete(slices.Clone(args), at, at+2)
}

// TestTranscodeOpensNoOtherInput (security): a library file that is a manifest
// or a playlist naming another audio file (a DASH MPD, an HLS playlist, an
// ffconcat script) is no input, whatever ffmpeg is handed: nothing of the
// file it names is transcoded. Left to guess the demuxer, ffmpeg followed
// each through the file protocol (the control, run on the manifest under its
// own extension, which skips the case where this ffmpeg reads no such
// manifest: no libxml2 for DASH) and sent that file's audio: another
// library's, for whoever could put the crafted file in one and stream it.
// Denied with the list in place: the same command line, then the file under
// an audio name through /dev/fd/3 (an ffmpeg without fd:), fd:, pipe:0 (a
// podcast episode on that ffmpeg) and the watch rendition. Allowed: the
// library's own formats (TestAllowListReadsEveryLibraryFormat).
func TestTranscodeOpensNoOtherInput(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The files the manifests name (DASH's an MP4): in the library's tree for
	// the ffconcat script, whose names may only be relative ones below it.
	other := filepath.Join(root, "Other", "secret.mp3")
	otherMP4 := filepath.Join(root, "Other", "secret.m4a")
	if err := os.Mkdir(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if !writeFormat(other, "-c:a", "libmp3lame", "-b:a", "64k") || !writeFormat(otherMP4, "-c:a", "aac", "-b:a", "64k") {
		t.Skip("ffmpeg could not write the audio the manifests name")
	}
	fileURL := func(p string) string { return "file://" + filepath.ToSlash(p) }
	noFD := pipeOnlyFFmpeg(t)
	for _, c := range []struct{ name, ext, body string }{
		{"DASH", ".mpd", `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011" type="static" mediaPresentationDuration="PT4S" minBufferTime="PT1S">
 <Period><AdaptationSet mimeType="audio/mp4" contentType="audio">
  <Representation id="a" bandwidth="64000" codecs="mp4a.40.2" audioSamplingRate="22050"><BaseURL>` + html.EscapeString(fileURL(otherMP4)) + `</BaseURL></Representation>
 </AdaptationSet></Period>
</MPD>
`},
		{"HLS", ".m3u8", "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.0,\n" + fileURL(other) + "\n#EXT-X-ENDLIST\n"},
		{"ffconcat", ".ffconcat", "ffconcat version 1.0\nfile Other/secret.mp3\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			natural := filepath.Join(root, "manifest"+c.ext)
			crafted := filepath.Join(root, c.name+".mp3") // as a library would index it
			for _, p := range []string{natural, crafted} {
				if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			listed := transcodeArgs(inputSpec{proto: "file", url: "file:" + natural}, 0)
			if out, _ := exec.Command("ffmpeg", withoutAllowList(t, listed)...).Output(); len(out) == 0 {
				t.Skipf("this ffmpeg follows no %s manifest to a local file", c.name)
			}
			if out, _ := exec.Command("ffmpeg", listed...).Output(); len(out) != 0 {
				t.Errorf("the manifest through the file protocol was transcoded: %d bytes of the file it names, want none", len(out))
			}
			ffmpegs := map[string]string{"/dev/fd/3 or a path": noFD}
			if inputFor("ffmpeg") == fdInput {
				ffmpegs["fd:"] = "ffmpeg"
			}
			for name, ffmpeg := range ffmpegs {
				rec := httptest.NewRecorder()
				if _, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), root, crafted, ffmpeg, 0, false, time.Minute, 0, nil); err != nil {
					t.Fatalf("%s: Transcode = %v, want it answered here", name, err)
				}
				if rec.Body.Len() != 0 {
					t.Errorf("%s: a manifest under an audio name was transcoded: %d bytes of the file it names, want none", name, rec.Body.Len())
				}
			}
			f, err := os.Open(crafted)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			rec := httptest.NewRecorder()
			TranscodeOpened(rec, httptest.NewRequest(http.MethodGet, "/", nil), f, noFD, 0, false, 0, nil)
			if rec.Body.Len() != 0 {
				t.Errorf("pipe:0: a manifest as an episode was transcoded: %d bytes, want none", rec.Body.Len())
			}
			m, err := NewRenditionCache(filepath.Join(t.TempDir(), "watch"), nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if copied, _, err := m.Open(ctx, ctx, "ffmpeg", root, crafted); !errors.Is(err, ErrRefused) {
				if copied != nil {
					copied.Close()
				}
				t.Errorf("a manifest's watch rendition = %v, want it refused", err)
			}
		})
	}
}

// writeFormat writes four seconds of a tone at path with ffmpeg's enc
// arguments (the muxer its extension names unless they name one), and reports
// whether this ffmpeg could (it may lack an encoder).
func writeFormat(path string, enc ...string) bool {
	args := append([]string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=440:r=22050", "-t", "4"}, enc...)
	return exec.Command("ffmpeg", append(args, "-y", path)...).Run() == nil
}

// TestAllowListReadsEveryLibraryFormat: the demuxer allow-list (audioDemuxers)
// refuses none of the library's own formats (metadata.AudioExtensions), nor a
// plain audio file under another's name: each still transcodes whole and from
// a seek, through fd: and through the /dev/fd/3 fallback of an ffmpeg without
// it, and encodes to a watch rendition of the source's length. A format this
// ffmpeg can't write is skipped.
func TestAllowListReadsEveryLibraryFormat(t *testing.T) {
	t.Parallel()
	ffprobe := needFFmpeg(t)
	ffmpegs := map[string]string{"no fd:": pipeOnlyFFmpeg(t)}
	fd := inputFor("ffmpeg") == fdInput
	if fd {
		ffmpegs["fd:"] = "ffmpeg"
	}
	duration := func(t *testing.T, body []byte) float64 {
		t.Helper()
		out := filepath.Join(t.TempDir(), "out.mp3")
		if err := os.WriteFile(out, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return ffprobeOf(t, ffprobe, out).duration(t)
	}
	for _, c := range []struct {
		name  string
		enc   []string
		seeks bool // false: a stream whose seek ffmpeg only estimates, or lands a page early
	}{
		{"a.mp3", []string{"-c:a", "libmp3lame", "-b:a", "64k"}, true},
		{"a.m4a", []string{"-c:a", "aac", "-b:a", "64k"}, true},
		{"a.m4b", []string{"-c:a", "aac", "-b:a", "64k"}, true},
		{"a.mp4", []string{"-c:a", "aac", "-b:a", "64k"}, true},
		{"a.flac", []string{"-c:a", "flac"}, true},
		{"a.ogg", []string{"-c:a", "libvorbis"}, true},
		{"a.opus", []string{"-c:a", "libopus", "-ar", "48000"}, true},
		{"mp3.mka", []string{"-c:a", "libmp3lame", "-b:a", "64k"}, true},
		{"aac.mka", []string{"-c:a", "aac", "-b:a", "64k"}, true},
		{"flac in ogg.ogg", []string{"-c:a", "flac", "-f", "ogg"}, false},
		{"webm.mka", []string{"-c:a", "libopus", "-ar", "48000", "-f", "webm"}, true},
		{"adts.m4a", []string{"-c:a", "aac", "-b:a", "64k", "-f", "adts"}, false},
		{"wav.mp3", []string{"-c:a", "pcm_s16le", "-f", "wav"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := filepath.Join(t.TempDir(), c.name)
			if !writeFormat(src, c.enc...) {
				t.Skipf("this ffmpeg could not write %s", c.name)
			}
			for name, ffmpeg := range ffmpegs {
				for _, tc := range []struct{ start, want float64 }{{0, 4}, {3, 1}} {
					if tc.start > 0 && !c.seeks {
						continue
					}
					rec := httptest.NewRecorder()
					if _, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), filepath.Dir(src), src, ffmpeg, tc.start, false, time.Minute, 0, nil); err != nil || rec.Code != http.StatusOK || rec.Body.Len() == 0 {
						t.Fatalf("%s from %v s: %d with %d bytes (%v), want it transcoded", name, tc.start, rec.Code, rec.Body.Len(), err)
					}
					if d := duration(t, rec.Body.Bytes()); d < tc.want-0.5 || d > tc.want+0.5 {
						t.Errorf("%s: transcoded duration from %v s = %v, want about %v", name, tc.start, d, tc.want)
					}
				}
			}
			if !fd {
				return // pipe:0 reads no MP4 whose index sits at its end: renditionCheck's
			}
			m, err := NewRenditionCache(filepath.Join(t.TempDir(), "watch"), nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			copied, _, err := m.Open(ctx, ctx, "ffmpeg", filepath.Dir(src), src)
			if err != nil {
				t.Fatalf("watch rendition: %v", err)
			}
			defer copied.Close()
			if d := ffprobeOf(t, ffprobe, copied.Name()).duration(t); d < 3.5 || d > 4.5 {
				t.Errorf("watch rendition's duration = %v, want about 4", d)
			}
		})
	}
}
