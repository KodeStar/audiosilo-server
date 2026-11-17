package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mp3Frame is one MPEG-1 layer III frame at 44.1 kHz, stereo, of the bitrate
// whose index is bi (9: 128 kbit/s, 11: 192 kbit/s), its payload zeros (no
// sync word in it).
func mp3Frame(bi byte) []byte {
	h := uint32(0xFFFB0000) | uint32(bi)<<12
	m, ok := decodeMPA(h)
	if !ok {
		panic("bad frame header")
	}
	f := make([]byte, m.frameSize)
	binary.BigEndian.PutUint32(f, h)
	return f
}

// vbrFrames is n frames whose bitrate varies (VBR with no Xing/VBRI header,
// as lame -t or ffmpeg -write_xing 0 writes it).
func vbrFrames(n int) []byte {
	var out []byte
	for i := range n {
		out = append(out, mp3Frame([]byte{9, 11, 10, 9, 13}[i%5])...)
	}
	return out
}

// framesSeconds is n MPEG-1 layer III frames' length at 44.1 kHz, as the
// probe prints it.
func framesSeconds(n int) float64 {
	return printTime(rescale(int64(n)*1152, 1000000, 44100), 1, 1000000)
}

// countingReader counts the bytes read from r.
type countingReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

// probeBytes is the in-process probe's answer for data by FFmpeg 8.1's rules,
// and how many bytes it read.
func probeBytes(t *testing.T, ctx context.Context, data []byte, timeout time.Duration) (*probeResult, string, int64) {
	t.Helper()
	r := &countingReader{r: bytes.NewReader(data)}
	b := newBlockFile(r, int64(len(data)))
	b.ff = ff8
	p, reason := fastProbe(ctx, b, "mp3", timeout)
	return p, reason, r.n.Load()
}

// A VBR MP3 with no Xing/VBRI header has its frames counted (SRV-16): every
// one, past junk in the middle, to the trailing ID3v1 and APEv2 tags (a frame
// in the APE tag is not audio), a last frame cut off by the end dropped.
func TestCountsHeaderlessVBRFrames(t *testing.T) {
	t.Parallel()
	const n = 3000 // ~78 s, a little over 1 MiB: the count's window moves
	stream := vbrFrames(n)
	id3v1 := append([]byte("TAG"), make([]byte, 125)...)
	// An APEv2 tag with its header, an item holding two frames in a row.
	item := append(mp3Frame(9), mp3Frame(9)...)
	apeSize := uint32(len(item) + 32) // the items and the footer
	apeHead := func(header bool) []byte {
		f := make([]byte, 32)
		copy(f, "APETAGEX")
		binary.LittleEndian.PutUint32(f[8:], 2000)
		binary.LittleEndian.PutUint32(f[12:], apeSize)
		binary.LittleEndian.PutUint32(f[16:], 1)
		flags := uint32(1 << 31) // it has a header
		if header {
			flags |= 1 << 29
		}
		binary.LittleEndian.PutUint32(f[20:], flags)
		return f
	}
	ape := append(append(apeHead(true), item...), apeHead(false)...)
	junk := bytes.Repeat([]byte{0x55}, 300)

	for _, tc := range []struct {
		name   string
		data   []byte
		frames int
	}{
		{"bare", stream, n},
		{"id3v1", append(append([]byte{}, stream...), id3v1...), n},
		{"ape and id3v1", append(append(append([]byte{}, stream...), ape...), id3v1...), n},
		{"junk in the middle", append(append(append([]byte{}, stream[:len(stream)/2]...), junk...), stream[len(stream)/2:]...), n},
		{"last frame cut off", stream[:len(stream)-100], n - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, reason, _ := probeBytes(t, t.Context(), tc.data, maxProbeTime)
			if reason != "" {
				t.Fatalf("left to ffprobe: %s", reason)
			}
			if want := framesSeconds(tc.frames); !p.Counted || p.Duration != want || p.Codec != "mp3" {
				t.Errorf("duration %v counted %t codec %q, want %v counted (%d frames)", p.Duration, p.Counted, p.Codec, want, tc.frames)
			}
		})
	}
}

// A header-less MP3 whose sampled frames share one bitrate keeps the
// constant bitrate's estimate (right for true CBR), with no count: only the
// frames sampled are read, not the file.
func TestHeaderlessCBRIsNotCounted(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat(mp3Frame(9), 20000) // 8 MiB
	p, reason, read := probeBytes(t, t.Context(), data, maxProbeTime)
	if reason != "" || p.Counted {
		t.Fatalf("reason %q counted %v, want the estimate", reason, p != nil && p.Counted)
	}
	if want := printTime(rescale(int64(len(data)), 8*14112000, 128000), 1, 14112000); math.Abs(p.Duration-want) > 1e-5 {
		t.Errorf("duration %v, want the bitrate's %v", p.Duration, want)
	}
	if read > int64(len(data))/4 {
		t.Errorf("read %d of %d bytes, want the sampled frames only", read, len(data))
	}
}

// A VBR file with a Xing header is never counted: its frame count is the
// header's, and the frames are not read.
func TestXingFileIsNotCounted(t *testing.T) {
	t.Parallel()
	const n = 20000
	xing := mp3Frame(9)
	at := 4 + 32 // MPEG-1 stereo: the Xing tag's offset in its frame
	copy(xing[at:], "Xing")
	binary.BigEndian.PutUint32(xing[at+4:], 1) // flags: frames
	binary.BigEndian.PutUint32(xing[at+8:], n)
	data := append(xing, vbrFrames(n)...)
	p, reason, read := probeBytes(t, t.Context(), data, maxProbeTime)
	if reason != "" || p.Counted {
		t.Fatalf("reason %q counted %v, want the header's frame count", reason, p != nil && p.Counted)
	}
	if want := printTime(rescale(int64(n)*1152, 14112000, 44100), 1, 14112000); math.Abs(p.Duration-want) > 1e-6 {
		t.Errorf("duration %v, want the header's %v", p.Duration, want)
	}
	if read > int64(len(data))/4 {
		t.Errorf("read %d of %d bytes, want the header only", read, len(data))
	}
}

// hangBetween is a file whose reads starting in [from, to) never return while
// hang is open (a share that stopped answering part way through a count; the
// head and the tail the probe reads first answer).
type hangBetween struct {
	r        io.ReaderAt
	from, to int64
	hang     chan struct{}
}

func (f hangBetween) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.from && off < f.to {
		<-f.hang
	}
	return f.r.ReadAt(p, off)
}

// The count is bounded: by its own deadline (the timeout it is given, the
// ffprobe run's it replaces) and by the caller's context, a read that never
// returns given up on as the probe's are. Out of time, the reason is a
// deadline's (timedOutReason: with no ffprobe, the scan reads it again
// alone); the caller's context ended is not.
func TestCountIsBounded(t *testing.T) {
	t.Parallel()
	data := vbrFrames(20000) // ~8 MiB
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		ctx      time.Duration
		timedOut bool
	}{
		{"its deadline", 100 * time.Millisecond, time.Minute, true},
		{"the caller's context", time.Minute, 300 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hang := make(chan struct{})
			t.Cleanup(func() { close(hang) })
			b := newBlockFile(hangBetween{r: bytes.NewReader(data), from: 2 << 20, to: 6 << 20, hang: hang}, int64(len(data)))
			b.ff = ff8
			ctx, cancel := context.WithTimeout(t.Context(), tc.ctx)
			defer cancel()
			start := time.Now()
			p, reason := fastProbe(ctx, b, "mp3", tc.timeout)
			if p != nil || !strings.HasPrefix(reason, reasonUncounted+"; frame count: limit: ") {
				t.Fatalf("reason %q, want the count cut short", reason)
			}
			if timedOutReason(reason) != tc.timedOut {
				t.Errorf("reason %q: timed out %t, want %t", reason, !tc.timedOut, tc.timedOut)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("gave up after %v", took)
			}
		})
	}
}

// encodeHeaderless writes seconds of a tone under noise as a VBR MP3 with no
// Xing header (ffmpeg -write_xing 0, libmp3lame -q:a 4), and returns its path
// and the length ffmpeg decodes it to; skipped without ffmpeg and libmp3lame.
func encodeHeaderless(t *testing.T, seconds int) (string, float64) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	path := filepath.Join(t.TempDir(), "vbr.mp3")
	src := "sine=frequency=440:duration=" + strconv.Itoa(seconds) + "[a];anoisesrc=d=" + strconv.Itoa(seconds) + ":a=0.3[n];[a][n]amix"
	out, err := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-filter_complex", src,
		"-ar", "44100", "-c:a", "libmp3lame", "-q:a", "4", "-write_xing", "0", path).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg can't write the fixture (libmp3lame?): %v %s", err, out)
	}
	pcm, err := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-ac", "1", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	return path, float64(len(pcm)/2) / 44100
}

// A real encoder's header-less VBR file reads as the length it decodes to,
// within one frame, where ffprobe's estimate is seconds off (SRV-16: 139.42 s
// for 150.05 s of audio): through Extract with no ffprobe, and with an ffprobe
// whose release the probe has no rules for (its estimate replaced).
func TestHeaderlessVBRReadsAsDecoded(t *testing.T) {
	t.Parallel()
	path, decoded := encodeHeaderless(t, 30)
	frame := 1152.0 / 44100
	md, err := Extract(path, "")
	if err != nil || md.Duration == 0 || math.Abs(md.Duration-decoded) > frame {
		t.Errorf("no ffprobe: %v duration %v, want %v within a frame", err, md.Duration, decoded)
	}
	probe, log := fakeFFprobe(t, "6.1.1", false)
	md, err = Extract(path, probe)
	if err != nil || math.Abs(md.Duration-decoded) > frame || md.Codec != "fake" {
		t.Errorf("an unknown ffprobe: %v duration %v codec %q, want %v counted over its answer", err, md.Duration, md.Codec, decoded)
	}
	if n := runs(t, log); n != 1 {
		t.Errorf("ffprobe read %d files, want 1", n)
	}
	// Any other file keeps that ffprobe's answer.
	md, _ = Extract(fixture("lame-cbr-noxing.mp3"), probe)
	if md.Duration != 1 {
		t.Errorf("header-less CBR with an unknown ffprobe: duration %v, want ffprobe's", md.Duration)
	}
}

// BenchmarkCountFile times the count of a long header-less VBR MP3: set
// AUDIOSILO_COUNT_FILE to one (a 10 h book, say) and run it with -bench.
func BenchmarkCountFile(bm *testing.B) {
	path := os.Getenv("AUDIOSILO_COUNT_FILE")
	if path == "" {
		bm.Skip("AUDIOSILO_COUNT_FILE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		bm.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		bm.Fatal(err)
	}
	bm.SetBytes(info.Size())
	for bm.Loop() {
		b := newBlockFile(f, info.Size())
		b.ff = ff8
		p, reason := fastProbe(bm.Context(), b, "mp3", ProbeTimeout)
		if reason != "" || !p.Counted {
			bm.Fatalf("%s: reason %q, counted %t; want a header-less VBR MP3 counted", path, reason, p != nil && p.Counted)
		}
	}
}
