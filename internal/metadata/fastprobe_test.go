package metadata

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		{"ffprobe version n8.1.3-25-g611cec792a-20261029 Copyright (c) 2007-2026 the FFmpeg developers", ff8}, // BtbN's n8.1-latest (toolfetch)
		{"ffprobe version n8.1-7-g1a2b3c4d5e-20261028 Copyright (c) 2007-2026", ff8},
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
	if _, reason := fastProbe(context.Background(), b, "mp3", maxProbeTime); reason != "" {
		t.Fatalf("the whole file: %s", reason)
	}
	b = newBlockFile(f, info.Size()*2)
	b.ff = ff7
	if p, reason := fastProbe(context.Background(), b, "mp3", maxProbeTime); p != nil || !strings.HasPrefix(reason, "read: ") {
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
	d.del("C") // the last entry, A, takes c's slot: its index moves with it
	for k, want := range map[string]string{"a": "4", "B": "2"} {
		if v, ok := d.get(k); !ok || v != want {
			t.Errorf("get(%q) = %q %v, want %q", k, v, ok, want)
		}
	}
	if _, ok := d.get("c"); ok || len(d.e) != 2 {
		t.Errorf("c deleted: %v", d.e)
	}
}

// ffprobe's string validation: one U+FFFD for each sequence av_utf8_decode
// rejects. An overlong form, a surrogate, a non-character or a code past
// U+10FFFF takes its whole sequence; a bad or missing continuation byte only
// the lead byte.
func TestValidUTF8(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain é ✓": "plain é ✓",
		"A\xc0\xafB\xed\xa0\x80C\xef\xbf\xbeD\xf8\x88\x80\x80\x80E\xe9F": "A\uFFFDB\uFFFDC\uFFFDD\uFFFDE\uFFFDF",
		"x\xe9":    "x\uFFFD",
		"\xff\xbf": "\uFFFD\uFFFD",
		"a\uFFFFb": "a\uFFFDb",
	} {
		if got := validUTF8(in); got != want {
			t.Errorf("validUTF8(%q) = %q, want %q", in, got, want)
		}
	}
}

// avpriv_new_chapter drops a chapter that ends before it starts, and
// compute_chapters_end ends an open chapter at the next one in start order
// only: one sharing its start leaves it to the format's end.
func TestChapterListRules(t *testing.T) {
	t.Parallel()
	var l chapterList
	l.add(0, 1, 1000, 0, 0, false, "a")
	l.add(1, 1, 1000, 0, 0, false, "b")
	l.add(2, 1, 1000, 2000, 0, false, "c")
	if c := l.add(3, 1, 1000, 3000, 1000, true, "backwards"); c != nil {
		t.Errorf("a chapter ending before its start was added: %+v", c)
	}
	var got []string
	for _, c := range l.finish(5000000, 0, false) {
		got = append(got, fmt.Sprintf("%s %g-%g", c.Title, c.Start, c.End))
	}
	if strings.Join(got, "|") != "a 0-5|b 0-2|c 2-5" {
		t.Errorf("chapters %q", got)
	}
}

// zlib's uncompress: the whole stream with its checksum, in no more than the
// declared size; anything else and FFmpeg skips the frame.
func TestUncompress(t *testing.T) {
	t.Parallel()
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	_, _ = w.Write([]byte("hello world"))
	_ = w.Close()
	zb := z.Bytes()
	if u, ok := uncompress(zb, 11); !ok || string(u) != "hello world" {
		t.Errorf("exact size: %q %v", u, ok)
	}
	if u, ok := uncompress(zb, 100); !ok || string(u) != "hello world" {
		t.Errorf("declared larger: %q %v", u, ok)
	}
	if u, ok := uncompress(zb, 10); ok {
		t.Errorf("declared smaller: %q, want a failure", u)
	}
	if u, ok := uncompress(zb[:len(zb)-4], 11); ok {
		t.Errorf("no checksum: %q, want a failure", u)
	}
}

// read_ttag's genre test is sscanf's %d into an unsigned int: strtol's value
// (clamped when out of range) cut to 32 bits, then at most 191.
func TestID3Genre(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{"12": 12, " 12": 12, "(13": 13, "(13)Pop": 13, "4294967297": 1, "(4294967298)": 2, "-99999999999999999999": 0} {
		if g, ok := id3Genre(in); !ok || g != want {
			t.Errorf("id3Genre(%q) = %d %v, want %d", in, g, ok, want)
		}
	}
	for _, in := range []string{"192", "(-1)", "-1", "99999999999999999999", "Pop", ""} {
		if g, ok := id3Genre(in); ok {
			t.Errorf("id3Genre(%q) = %d, want no genre", in, g)
		}
	}
}

// A chapter track's tables are read only as far as they go and only in the
// shape FFmpeg reads without repairing: anything else, or too many samples,
// leaves the file to ffprobe rather than walking billions of chunks or samples.
func TestMP4ChapterTrackBounds(t *testing.T) {
	t.Parallel()
	words := func(w ...uint32) []byte {
		b := make([]byte, 4*len(w))
		for i, x := range w {
			binary.BigEndian.PutUint32(b[4*i:], x)
		}
		return b
	}
	for name, tc := range map[string]struct {
		stsz, stsc, stco []byte
		want             string
	}{
		"a chunk count past stco":    {words(0, 16, 1), words(0, 1, 1, 1, 1), words(0, 0xffffffff, 100), "mp4: chapter track tables"},
		"an stsc count past stsc":    {words(0, 16, 1), words(0, 0xffffffff, 1, 1, 1), words(0, 1, 100), "mp4: chapter track tables"},
		"an stsc not from chunk 1":   {words(0, 16, 1), words(0, 1, 0x7fffffff, 1, 1), words(0, 1, 100), "mp4: chapter track stsc"},
		"an stsc entry of 0 samples": {words(0, 16, 1), words(0, 1, 1, 0, 1), words(0, 1, 100), "mp4: chapter track stsc"},
		"billions of fixed samples":  {words(0, 16, 0x7fffffff), words(0, 1, 1, 0x7fffffff, 1), words(0, 1, 100), "mp4: chapter track with too many samples"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var file []byte
			leaf := func(b []byte) mp4Leaf {
				l := mp4Leaf{int64(len(file)), int64(len(b))}
				file = append(file, b...)
				return l
			}
			tr := &mp4Track{timescale: 1000, stts: [][2]uint32{{1, 1000}}}
			tr.stsz, tr.stsc, tr.stco = leaf(tc.stsz), leaf(tc.stsc), leaf(tc.stco)
			p := filepath.Join(t.TempDir(), "tables")
			if err := os.WriteFile(p, file, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			s := &mp4State{b: newBlockFile(f, int64(len(file)))}
			if err := s.readChapterTrack(tr); err == nil || err.Error() != tc.want {
				t.Errorf("%v, want %s", err, tc.want)
			}
		})
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

// freeBoxes is an MP4 of nothing but free boxes of every bytes, size long:
// each box header the walk reads costs it a block, so a big one reads past the
// probe's budget without ever finding a moov. delay slows every read.
type freeBoxes struct {
	every, size int64
	delay       time.Duration
}

// freeHeader is a free box's header, size long.
func freeHeader(size int64) []byte {
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h[:4], uint32(size))
	copy(h[4:], "free")
	return h
}

func (r freeBoxes) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(r.delay)
	clear(p)
	h := freeHeader(r.every)
	for box := off / r.every * r.every; box < off+int64(len(p)); box += r.every {
		for i := range h {
			if at := box + int64(i) - off; at >= 0 && at < int64(len(p)) {
				p[at] = h[i]
			}
		}
	}
	return len(p), nil
}

// The in-process probe is bounded: a file that would read past its byte
// budget, a reader too slow for its deadline, or the caller's ended context
// stops it with a limit reason (Extract then asks ffprobe), never a hang.
func TestFastProbeLimits(t *testing.T) {
	t.Parallel()
	const gib = 1 << 30
	probeWith := func(ctx context.Context, r freeBoxes, timeout time.Duration) (*probeResult, string) {
		b := newBlockFile(r, r.size)
		b.ff = ff8
		return fastProbe(ctx, b, "m4b", timeout)
	}
	// 2 GiB of 128 KiB boxes is 16384 headers, one 64 KiB block each: 1 GiB.
	if p, reason := probeWith(context.Background(), freeBoxes{every: 128 << 10, size: 2 * gib}, time.Minute); p != nil || reason != "limit: probe read budget" {
		t.Errorf("past the budget: %+v, %q", p, reason)
	}
	// The same shape well inside it is simply unsure (no moov).
	if p, reason := probeWith(context.Background(), freeBoxes{every: 128 << 10, size: 16 << 20}, time.Minute); p != nil || reason != "mp4: no moov" {
		t.Errorf("inside the budget: %+v, %q", p, reason)
	}
	// 10 ms a read and 1 MiB boxes: the 100 ms deadline trips after ~10 reads.
	start := time.Now()
	if p, reason := probeWith(context.Background(), freeBoxes{every: 1 << 20, size: 2 * gib, delay: 10 * time.Millisecond}, 100*time.Millisecond); p != nil || reason != "limit: probe deadline" {
		t.Errorf("a slow reader: %+v, %q", p, reason)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a slow reader took %v to give up", took)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, reason := probeWith(ctx, freeBoxes{every: 1 << 20, size: gib}, time.Minute); p != nil || reason != "limit: context canceled" {
		t.Errorf("an ended context: %+v, %q", p, reason)
	}
}

// A loop that goes round without reading checks the deadline too, every 256
// ticks; disarmed (outside fastProbe) a tick never trips.
func TestBlockFileTick(t *testing.T) {
	t.Parallel()
	b := newBlockFile(bytes.NewReader(nil), 0)
	for range 1000 {
		b.tick()
	}
	ctx, cancel := context.WithTimeoutCause(context.Background(), -time.Second, errProbeDeadline)
	defer cancel()
	b.arm(ctx, maxProbeBytes)
	defer func() {
		if r := recover(); r != probeLimit("limit: probe deadline") {
			t.Errorf("recovered %v, want the deadline", r)
		}
	}()
	for range 256 {
		b.tick()
	}
	t.Error("256 ticks past the deadline never tripped it")
}

// A file past the probe's budget is read by ffprobe instead, with no error of
// its own: the limit is the probe's, not the file's.
func TestExtractFallsBackPastTheProbeBudget(t *testing.T) {
	t.Parallel()
	probe, log := fakeFFprobe(t, "8.1.3", false)
	path := filepath.Join(t.TempDir(), "huge.m4b")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse 256 MiB of 128 KiB free boxes: 2048 headers, 128 MiB of blocks.
	const every, size = 128 << 10, 256 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	h := freeHeader(every)
	for at := int64(0); at < size; at += every {
		if _, err := f.WriteAt(h, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	md, err := Extract(path, probe)
	if err != nil || md.ProbeErr != nil {
		t.Fatalf("Extract: %v, %v", err, md.ProbeErr)
	}
	if md.Duration != 1 || md.Codec != "fake" {
		t.Errorf("duration %v codec %q, want ffprobe's answer", md.Duration, md.Codec)
	}
	if n := runs(t, log); n != 1 {
		t.Errorf("ffprobe read %d files, want the one past the budget", n)
	}
}

// failReader fails its test when read.
type failReader struct{ t *testing.T }

func (r failReader) ReadAt([]byte, int64) (int, error) {
	r.t.Error("read past the budget")
	return 0, io.EOF
}

// A read the budget can't cover is refused before its buffer is made (an MKV's
// EBML header can name a size up to the file's).
func TestBlockFileBudgetBeforeAllocation(t *testing.T) {
	t.Parallel()
	b := newBlockFile(failReader{t}, 1<<40)
	b.arm(context.Background(), maxProbeBytes)
	defer func() {
		if r := recover(); r != probeLimit("limit: probe read budget") {
			t.Errorf("recovered %v, want the budget", r)
		}
	}()
	b.bytesAt(0, 1<<39)
	t.Error("a read past the budget was made")
}
