package metadata

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/textclean"
)

// A library file is read by ffprobe as an audio container only
// (InputFormatArgs). One shaped like a playlist under an audio name, which
// ffprobe would otherwise read as the files it lists, is an error, and the
// file it names is never opened, and the refusal is a rejection of the file's
// data (ErrProbeRejected: not probed again while the file is unchanged); the
// containers the server indexes still probe (duration, codec, chapters, tags).
func TestProbeReadsNoPlaylist(t *testing.T) {
	t.Parallel()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on PATH")
	}
	dir := t.TempDir()
	secret, err := os.ReadFile(fixture("lame-tags.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	playlists := map[string]string{
		"concat.mp3": "ffconcat version 1.0\nfile secret.mp3\n",
		"hls.mp3":    "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nsecret.mp3\n#EXT-X-ENDLIST\n",
		"hls.m3u8":   "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nsecret.mp3\n#EXT-X-ENDLIST\n",
	}
	for name, body := range playlists {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.mp3"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	// ffprobe by the file's path, as nothing in the server runs it: the plain
	// run shows what the list is there for (the listed file's stream read),
	// the run with the list reads none of it.
	streams := func(name string, args ...string) (string, error) {
		args = append(args, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", filepath.Join(dir, name))
		var out bytes.Buffer
		cmd := exec.CommandContext(t.Context(), ffprobe, args...)
		cmd.Stdout = &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	for name := range playlists {
		plain, _ := streams(name)
		got, err := streams(name, InputFormatArgs()...)
		if err == nil || got != "" {
			t.Errorf("%s: ffprobe held to InputFormats answered %q (%v), want an error and nothing read (without the list: %q)", name, got, err, plain)
		}
	}
	if plain, err := streams("concat.mp3"); err != nil || !strings.HasPrefix(plain, "mp3") {
		t.Logf("this ffprobe does not read a concat script under an audio name (%q, %v): the list is not what refuses it here", plain, err)
	}
	// The server's own probe, the file handed to ffprobe opened.
	for name := range playlists {
		if p, err := probe(context.Background(), dir, filepath.Join(dir, name), ffprobe, ProbeTimeout); !errors.Is(err, ErrProbeRejected) {
			t.Errorf("%s: probed as %+v (%v), want a rejection", name, p, err)
		}
	}
	for _, tc := range []struct {
		file     string
		codec    string
		chapters int
		title    string
	}{
		{"m4b-aac-chap.m4b", "aac", 3, "The Spike Book (Unabridged)"},
		{"lame-tags.mp3", "mp3", 0, ""},
		{"flac-chap.flac", "flac", 0, ""},
		{"mka-mp3-chap.mka", "mp3", 1, ""},
		{"vorbis-chap.ogg", "vorbis", 0, ""},
		{"opus-chap.opus", "opus", 1, ""},
	} {
		p, err := probe(context.Background(), probeFixtures, fixture(tc.file), ffprobe, ProbeTimeout)
		if err != nil {
			t.Errorf("%s: %v", tc.file, err)
			continue
		}
		if p.Duration <= 0 || p.Codec == "" || (tc.codec != "" && p.Codec != tc.codec) || len(p.Chapters) < tc.chapters {
			t.Errorf("%s: duration %v, codec %q, %d chapters; want a duration, codec %q and at least %d chapters", tc.file, p.Duration, p.Codec, len(p.Chapters), tc.codec, tc.chapters)
		}
		if got := textclean.FirstNonEmpty(p.Tags["album"], p.Tags["title"]); tc.title != "" && got != tc.title {
			t.Errorf("%s: title %q, want %q", tc.file, got, tc.title)
		}
	}
}

// A book in another audio container under a name the server indexes (AIFF,
// WMA's ASF, AC-3, Wave64, E-AC-3, WavPack, CAF) still probes held to
// InputFormats, as it did before the list; every name on the list is a
// demuxer this ffmpeg has (a misspelt one would refuse its format).
func TestProbeReadsOtherAudioContainers(t *testing.T) {
	t.Parallel()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on PATH")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	out, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-demuxers").Output()
	if err != nil {
		t.Fatalf("ffmpeg -demuxers: %v", err)
	}
	demuxers := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			for name := range strings.SplitSeq(f[1], ",") {
				demuxers[name] = true
			}
		}
	}
	for name := range strings.SplitSeq(InputFormats, ",") {
		if !demuxers[name] {
			t.Errorf("InputFormats names %q, a demuxer ffmpeg -demuxers doesn't list", name)
		}
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		file, format, codec string
	}{
		{"aiff.m4a", "aiff", "pcm_s16be"},
		{"wma.mp3", "asf", "wmav2"},
		{"ac3.m4b", "ac3", "ac3"},
		{"w64.flac", "w64", "pcm_s16le"},
		{"eac3.mp3", "eac3", "eac3"},
		{"wv.ogg", "wv", "wavpack"},
		{"caf.m4a", "caf", "pcm_s16le"},
	} {
		path := filepath.Join(dir, tc.file)
		gen := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=d=2",
			"-c:a", tc.codec, "-f", tc.format, path)
		if out, err := gen.CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", tc.file, err, out)
		}
		p, err := probe(context.Background(), dir, path, ffprobe, ProbeTimeout)
		if err != nil {
			t.Errorf("%s: %v", tc.file, err)
			continue
		}
		if p.Duration <= 0 || p.Codec != tc.codec {
			t.Errorf("%s: duration %v, codec %q; want a duration and codec %q", tc.file, p.Duration, p.Codec, tc.codec)
		}
	}
}

// ffprobe is run held to InputFormats, the list given before the input it
// applies to.
func TestProbePassesInputFormats(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stand-in is a shell script")
	}
	dir := t.TempDir()
	stand, args := filepath.Join(dir, "ffprobe"), filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\necho '{\"format\":{\"duration\":\"1.000000\"}}'\n"
	writeStandIn(t, stand, script)
	file := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probe(context.Background(), dir, file, stand, ProbeTimeout); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	at := slices.Index(got, "-format_whitelist")
	if at < 0 || at+2 != len(got)-1 || got[at+1] != InputFormats {
		t.Fatalf("ffprobe's arguments %q, want -format_whitelist %s and then the input", got, InputFormats)
	}
}

// writeStandIn writes script, a tool's stand-in, at path to be run. The fork
// lock is held so no exec another parallel test forks meanwhile inherits the
// file still open for writing (ETXTBSY on Linux, golang/go#22315).
func writeStandIn(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
}
