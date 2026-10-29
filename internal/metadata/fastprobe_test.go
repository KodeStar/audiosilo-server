package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixture(name string) string { return filepath.Join(probeFixtures, name) }

// An ffprobe's -version names the rules the probe follows: a release it knows
// by major.minor, whatever the patch or the distribution's suffix; anything
// else (an older or newer release, a master build, not ffprobe) none.
func TestRulesFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		want    ffRules
	}{
		{"ffprobe version 7.1.1 Copyright (c) 2007-2025 the FFmpeg developers\nbuilt with", ff7},
		{"ffprobe version 7.1.1-3ubuntu1 Copyright (c) 2007-2025", ff7},
		{"ffprobe version 7.1 Copyright", ff7},
		{"ffprobe version 8.1.3 Copyright (c) 2007-2026 the FFmpeg developers", ff8},
		{"ffprobe version n8.1.3-12-gdeadbeef Copyright", ff8},
		{"ffprobe version 8.1-static https://johnvansickle.com", ff8},
		{"ffprobe version 8.0 Copyright", ffNone},
		{"ffprobe version 7.0.2 Copyright", ffNone},
		{"ffprobe version 6.1.1-3ubuntu5 Copyright", ffNone},
		{"ffprobe version 9.0 Copyright", ffNone},
		{"ffprobe version N-121510-g6e6a2b6f2c-20251028 Copyright", ffNone},
		{"ffmpeg version 8.1.3 Copyright", ffNone},
		{`{"format":{}}`, ffNone},
		{"", ffNone},
	} {
		if got := rulesFor(tc.version); got != tc.want {
			t.Errorf("rulesFor(%q) = %d, want %d", tc.version, got, tc.want)
		}
	}
}

// fakeFFprobe is an ffprobe stand-in naming version as its release, which
// appends each file it is asked to read to a log and answers a duration of 1 s
// and no chapters (exits 1 with ffmpeg's rejection when reject).
func fakeFFprobe(t *testing.T, version string, reject bool) (probe, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stand-in is a shell script")
	}
	dir := t.TempDir()
	probe, log = filepath.Join(dir, "ffprobe"), filepath.Join(dir, "log")
	answer := `echo '{"streams":[{"codec_name":"fake","codec_type":"audio"}],"format":{"duration":"1.000000"}}'`
	if reject {
		answer = `echo "[mov,mp4 @ 0x1] moov atom not found" >&2; echo "$f: Invalid data found when processing input" >&2; exit 1`
	}
	script := `#!/bin/sh
if [ "$1" = "-version" ]; then echo "ffprobe version ` + version + ` Copyright (c) 2007-2026"; exit 0; fi
for f; do :; done
echo "$f" >> '` + log + `'
` + answer + "\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
	return probe, log
}

// runs is how many files the stand-in was asked to read.
func runs(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// With an ffprobe of a release it knows, Extract answers in-process (the
// fixture's own duration and chapters, the stand-in never run); for a file it
// is unsure of, ffprobe reads it and a failure is ffprobe's own error, as the
// Health page shows it.
func TestExtractProbesInProcess(t *testing.T) {
	t.Parallel()
	probe, log := fakeFFprobe(t, "8.1.3", true)
	md, err := Extract(fixture("m4b-aac-chap.m4b"), probe)
	if err != nil || md.ProbeErr != nil {
		t.Fatalf("Extract: %v, %v", err, md.ProbeErr)
	}
	if md.Duration != 5.3 || len(md.Chapters) != 3 || md.Codec != "aac" || md.Chapters[1].Title != "Middle part é" {
		t.Errorf("duration %v, codec %q, chapters %+v; want the fixture's 5.3 s, aac, three chapters", md.Duration, md.Codec, md.Chapters)
	}
	if md.Title != "The Spike Book (Unabridged)" || md.Narrator != "Nick Narrator" {
		t.Errorf("title %q narrator %q", md.Title, md.Narrator)
	}
	if n := runs(t, log); n != 0 {
		t.Errorf("ffprobe read %d files, want none", n)
	}
	md, _ = Extract(fixture("craft-m4b-truncated.m4b"), probe)
	if !errors.Is(md.ProbeErr, ErrProbeRejected) || !strings.Contains(md.ProbeErr.Error(), "moov atom not found") {
		t.Errorf("a file neither reads: ProbeErr %v, want ffprobe's rejection", md.ProbeErr)
	}
	if n := runs(t, log); n != 1 {
		t.Errorf("ffprobe read %d files, want the one the probe left to it", n)
	}
}

// An ffprobe whose release the probe knows no rules for reads every file, as
// before the probe; so does one ForceFFprobeForTests names, whose -version is
// never asked.
func TestExtractLeavesUnknownReleasesToFFprobe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version string
		force         bool
	}{
		{"older release", "6.1.1-3ubuntu5", false},
		{"master build", "N-121510-g6e6a2b6f2c-20251028", false},
		{"forced", "8.1.3", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, log := fakeFFprobe(t, tc.version, false)
			if tc.force {
				ForceFFprobeForTests(probe)
			}
			md, _ := Extract(fixture("mp3-v23-chap.mp3"), probe)
			if md.Duration != 1 || md.Codec != "fake" || len(md.Chapters) != 0 {
				t.Errorf("duration %v codec %q chapters %d: want ffprobe's answer", md.Duration, md.Codec, len(md.Chapters))
			}
			if n := runs(t, log); n != 1 {
				t.Errorf("ffprobe read %d files, want 1", n)
			}
		})
	}
}

// With ffprobe off ("") nothing is probed, in-process or not: tags only, no
// duration or chapters, as before the probe (SCAN-SPIKE decision 2).
func TestExtractWithoutFFprobeReadsTagsOnly(t *testing.T) {
	t.Parallel()
	md, err := Extract(fixture("m4b-aac-chap.m4b"), "")
	if err != nil || md.ProbeErr != nil {
		t.Fatal(err, md.ProbeErr)
	}
	if md.Duration != 0 || len(md.Chapters) != 0 || md.Codec != "" {
		t.Errorf("duration %v codec %q chapters %d, want none", md.Duration, md.Codec, len(md.Chapters))
	}
	if md.Title != "The Spike Book (Unabridged)" {
		t.Errorf("title %q, want the tags' title", md.Title)
	}
}

// A file that reads short of the size its stat gave (it shrank, or a read
// failed) is left to ffprobe, never answered from what was read.
func TestFastProbeReadFailureIsUnsure(t *testing.T) {
	t.Parallel()
	f, err := os.Open(fixture("mp3-cbr-bare.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	b := newBlockFile(f, info.Size())
	b.ff = ff7
	if _, reason := fastProbe(b, "mp3"); reason != "" {
		t.Fatalf("the whole file: %s", reason)
	}
	b = newBlockFile(f, info.Size()*2)
	b.ff = ff7
	if p, reason := fastProbe(b, "mp3"); p != nil || !strings.HasPrefix(reason, "read: ") {
		t.Errorf("a file shorter than its size: %+v, %q, want a read failure", p, reason)
	}
}

// The FFmpeg rules the probe reproduces, each on the fixture made for it.
func TestFastProbeRules(t *testing.T) {
	t.Parallel()
	view := func(t *testing.T, name string, ff ffRules) *ProbeView {
		t.Helper()
		v, reason, err := probeView(fixture(name), ff)
		if err != nil || reason != "" {
			t.Fatalf("%s: %v %s", name, err, reason)
		}
		return v
	}
	titles := func(v *ProbeView) []string {
		var out []string
		for _, c := range v.Chapters {
			out = append(out, c.Title)
		}
		return out
	}
	t.Run("MP4 duration is the shortest of mdhd, the samples and the edit list", func(t *testing.T) {
		t.Parallel()
		if d := view(t, "m4b-aac-chap.m4b", ff7).Duration; d != "5.300000" {
			t.Errorf("edit list: %s", d) // the edit list leaves out the encoder's priming
		}
		if d := view(t, "craft-m4b-noelst.m4b", ff7).Duration; d != "5.323220" {
			t.Errorf("no edit list: %s", d)
		}
		if d := view(t, "craft-m4b-mdhd-short.m4b", ff7).Duration; d != "3.323220" {
			t.Errorf("mdhd shorter than the samples: %s", d)
		}
	})
	t.Run("an M4B's chapter track overrides its Nero chpl", func(t *testing.T) {
		t.Parallel()
		if got := titles(view(t, "craft-m4b-chpl-differs.m4b", ff7)); strings.Join(got, "|") != "Opening|Middle part é|The End" {
			t.Errorf("titles %q, want the chapter track's", got)
		}
		if got := titles(view(t, "craft-m4b-chplonly.m4b", ff7)); len(got) != 3 {
			t.Errorf("chpl alone: %q", got)
		}
		if got := titles(view(t, "craft-m4b-chpl-more.m4b", ff7)); len(got) != 4 || got[3] != "c-four" {
			t.Errorf("a chpl with one more: %q, want the track's three and chpl's fourth", got)
		}
	})
	t.Run("ID3: the first frame and the first tag win, TYER+TDAT merge", func(t *testing.T) {
		t.Parallel()
		v := view(t, "craft-id3v23-full.mp3", ff7)
		if v.Tags["title"] != "Kapitel 1 – Anfang" || v.Tags["date"] != "2004-05-12" || v.Tags["genre"] != "Speech" || v.Tags["narrator"] != "Ned Reader" {
			t.Errorf("tags %v", v.Tags)
		}
		v = view(t, "craft-two-id3.mp3", ff7)
		if v.Tags["album"] != "First Tag Album" || v.Tags["artist"] != "Second Tag Artist" {
			t.Errorf("two tags: %v", v.Tags)
		}
		if v := view(t, "craft-v1-and-v2.mp3", ff7); v.Tags["artist"] != "" || v.Tags["title"] != "V2 Title" {
			t.Errorf("ID3v1 read beside an ID3v2: %v", v.Tags)
		}
		if got := titles(view(t, "craft-chap-unordered.mp3", ff7)); strings.Join(got, "|") != "Third ✓|First|c1" {
			t.Errorf("CHAP frames: %q, want file order, the element id for no title", got)
		}
	})
	t.Run("FFmpeg 8.1: gapless MP3 durations, empty UTF-16 strings", func(t *testing.T) {
		t.Parallel()
		if d7, d8 := view(t, "lame-vbr.mp3", ff7).Duration, view(t, "lame-vbr.mp3", ff8).Duration; d7 == d8 || d8 != "5.300000" {
			t.Errorf("7.1 %s, 8.1 %s: want 8.1 without the encoder delay and padding", d7, d8)
		}
		v7, v8 := view(t, "craft-id3-empty-utf16.mp3", ff7), view(t, "craft-id3-empty-utf16.mp3", ff8)
		if v7.Tags["title"] != "V1 Behind Empty" {
			t.Errorf("7.1 drops the frames and reads ID3v1: %v", v7.Tags)
		}
		if s, ok := v8.Tags["series"]; !ok || s != "" || v8.Tags["title"] != "" {
			t.Errorf("8.1 reads the frames as empty strings, so no ID3v1: %v", v8.Tags)
		}
	})
	t.Run("Ogg Vorbis: the start heuristic, no format tags", func(t *testing.T) {
		t.Parallel()
		if v := view(t, "vorbis-chap.ogg", ff7); v.Duration != "5.300000" || v.Tags != nil {
			t.Errorf("%+v", v)
		}
	})
	t.Run("FLAC: repeated comments join, a chapter with no name", func(t *testing.T) {
		t.Parallel()
		v := view(t, "craft-flac-chapters.flac", ff7)
		if v.Tags["artist"] != "Plain Artist;Second Artist" || strings.Join(titles(v), "|") != "Intro|Middle|" {
			t.Errorf("%+v", v)
		}
	})
	t.Run("Matroska: TagDefault 0 keeps a language tag off the plain key", func(t *testing.T) {
		t.Parallel()
		v := view(t, "craft-mka-a.mka", ff7)
		if v.Tags["title"] != "Info Title" || v.Tags["narrator"] != "" || v.Tags["series"] != "Craft Series" {
			t.Errorf("tags %v", v.Tags)
		}
		if got := titles(v); strings.Join(got, "|") != "One|Two (no end)|Four last wins" {
			t.Errorf("chapters %q", got)
		}
		if got := titles(view(t, "craft-mka-seekhead.mka", ff7)); len(got) != 2 {
			t.Errorf("chapters after the cluster, through the SeekHead: %q", got)
		}
	})
}

// FFmpeg's dictionary: keys match without ASCII case, a replaced entry's slot
// is taken by the last entry and the new value appended, DONT_OVERWRITE keeps
// the first.
func TestAVDict(t *testing.T) {
	t.Parallel()
	var d avDict
	d.set("a", "1", 0)
	d.set("b", "2", 0)
	d.set("c", "3", 0)
	d.set("A", "4", 0)
	d.set("B", "x", dictDontOverwrite)
	var got []string
	for _, e := range d.e {
		got = append(got, e.k+"="+e.v)
	}
	if strings.Join(got, ",") != "c=3,b=2,A=4" {
		t.Errorf("entries %v", got)
	}
}

// av_rescale_rnd's AV_ROUND_NEAR_INF: halves away from zero.
func TestRescale(t *testing.T) {
	t.Parallel()
	for _, tc := range [][4]int64{{3, 1, 2, 2}, {-3, 1, 2, -2}, {5, 2, 4, 3}, {1, 1, 3, 0}, {7, 0, 3, 0}, {1, 1, 0, 0}} {
		if got := rescale(tc[0], tc[1], tc[2]); got != tc[3] {
			t.Errorf("rescale(%d, %d, %d) = %d, want %d", tc[0], tc[1], tc[2], got, tc[3])
		}
	}
}
