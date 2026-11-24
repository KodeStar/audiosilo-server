package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	p, reason, _ := fastProbe(ctx, b, "mp3", timeout)
	return p, reason, r.n.Load()
}

// A VBR MP3 with no Xing/VBRI header has its frames counted (SRV-16): every
// one, past junk in the middle, to the trailing ID3v1 and APEv2 tags (a frame
// in the APE tag is not audio), a last frame cut off by the end counted, as
// ffmpeg decodes it (one with only its header left included).
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
		{"last frame cut off", stream[:len(stream)-100], n},
		{"only the last header left", stream[:len(stream)-len(mp3Frame(13))+4], n},
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

// mp3Frame48 is mp3Frame at 48 kHz.
func mp3Frame48(bi byte) []byte {
	h := uint32(0xFFFB0000) | uint32(bi)<<12 | 1<<10
	m, ok := decodeMPA(h)
	if !ok {
		panic("bad frame header")
	}
	f := make([]byte, m.frameSize)
	binary.BigEndian.PutUint32(f, h)
	return f
}

// A header-less VBR MP3 whose sample rate changes (files at 44.1 and 48 kHz
// joined) has each frame counted at its own rate (server#452): half and half,
// and a short run at the other rate, under the eighth a count skips as junk,
// which it once was (20.04 s read for 21.65 s).
func TestRateChangeIsCountedAtEachRate(t *testing.T) {
	t.Parallel()
	other := func(n int) []byte {
		var out []byte
		for i := range n {
			out = append(out, mp3Frame48([]byte{9, 11, 10}[i%3])...)
		}
		return out
	}
	seconds := func(n44, n48 int) float64 {
		return printTime(rescale(int64(n44)*1152, 1000000, 44100)+rescale(int64(n48)*1152, 1000000, 48000), 1, 1000000)
	}
	for _, tc := range []struct {
		name     string
		data     []byte
		n44, n48 int
	}{
		{"half and half", append(vbrFrames(1000), other(1000)...), 1000, 1000},
		{"a short run", append(append(vbrFrames(1500), other(100)...), vbrFrames(1500)...), 3000, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, reason, _ := probeBytes(t, t.Context(), tc.data, maxProbeTime)
			if reason != "" {
				t.Fatalf("left to ffprobe: %s", reason)
			}
			if want := seconds(tc.n44, tc.n48); !p.Counted || math.Abs(p.Duration-want) > 2e-6 {
				t.Errorf("duration %v counted %t, want %v counted", p.Duration, p.Counted, want)
			}
		})
	}
}

// A count that finds over an eighth of the audio is no frame (free-format
// frames, damage) gives up: ffprobe reads the file, and it is not marked
// uncounted, as a count that gave up reads the same every time.
func TestJunkPastAnEighthIsNotCounted(t *testing.T) {
	t.Parallel()
	stream := vbrFrames(1000)
	var data []byte
	data = append(data, stream[:len(stream)/2]...)
	data = append(data, bytes.Repeat([]byte{0x55}, len(stream)/4)...)
	data = append(data, stream[len(stream)/2:]...)
	b := newBlockFile(bytes.NewReader(data), int64(len(data)))
	b.ff = ff8
	p, reason, uncounted := fastProbe(t.Context(), b, "mp3", maxProbeTime)
	if p != nil || !strings.Contains(reason, "frames lost") {
		t.Fatalf("answer %+v, reason %q; want it left to ffprobe as frames lost", p, reason)
	}
	if uncounted {
		t.Error("marked uncounted: a count given up reads the same every time, so the scan would read the book on every run")
	}
}

// A header-less VBR MP3 whose frames differ only where the probe's old four
// samples (20% to 80% of the file) did not look, or whose first frame is at
// the lowest bitrate (a VBR encoder's silence: server#452, a silence-heavy file
// read as 86 s for 60 s), is counted, not taken for a constant bitrate.
func TestVBRSampledAsCBRIsCounted(t *testing.T) {
	t.Parallel()
	frames := func(base, other byte, n, from, to int) []byte {
		var out []byte
		for i := range n {
			bi := base
			if i >= from && i < to {
				bi = other
			}
			out = append(out, mp3Frame(bi)...)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		// 128 kbit/s but for 192 kbit/s frames around the middle.
		{"varies between the old samples", frames(9, 11, 6000, 2900, 3100)},
		// 32 kbit/s (silence) but for a burst of 128 kbit/s frames no sample hits.
		{"silence at the lowest bitrate", frames(1, 9, 6000, 1000, 1010)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, reason, _ := probeBytes(t, t.Context(), tc.data, maxProbeTime)
			if reason != "" {
				t.Fatalf("left to ffprobe: %s", reason)
			}
			if want := framesSeconds(6000); !p.Counted || p.Duration != want {
				t.Errorf("duration %v counted %t, want %v counted", p.Duration, p.Counted, want)
			}
		})
	}
}

// A real encoder's VBR file that is mostly silence, its first frame and most
// sampled ones silence at the lowest bitrate, reads as the length it decodes
// to (server#452); skipped without ffmpeg and libmp3lame.
func TestSilenceHeavyVBRReadsAsDecoded(t *testing.T) {
	t.Parallel()
	path, decoded := encodeMP3(t, "sine=frequency=440:duration=60,volume='if(gt(mod(t,10),8.5),1,0)':eval=frame",
		"-q:a", "2", "-write_xing", "0")
	md, err := Extract(path, "")
	if err != nil || math.Abs(md.Duration-decoded) > 1152.0/44100 {
		t.Errorf("%v duration %v, want %v within a frame", err, md.Duration, decoded)
	}
}

// A Xing header that gives the audio's bytes but no frame count has its
// frames counted (server#452), where ffprobe estimates from the first frame's
// bitrate: every frame past the Xing frame, less what the decoder skips at
// the start when an encoder tag gives its delay.
func TestXingWithoutFrameCountIsCounted(t *testing.T) {
	t.Parallel()
	const n = 3000
	stream := vbrFrames(n)
	xing := mp3Frame(9)
	at := 4 + 32 // MPEG-1 stereo: the Xing tag's offset in its frame
	copy(xing[at:], "Xing")
	binary.BigEndian.PutUint32(xing[at+4:], 2) // flags: bytes
	binary.BigEndian.PutUint32(xing[at+8:], uint32(len(stream)+len(xing)))
	p, reason, _ := probeBytes(t, t.Context(), append(xing, stream...), maxProbeTime)
	if reason != "" {
		t.Fatalf("left to ffprobe: %s", reason)
	}
	if want := framesSeconds(n); !p.Counted || p.Duration != want {
		t.Errorf("duration %v counted %t, want %v counted", p.Duration, p.Counted, want)
	}

	// A real encoder's, its frame count taken out: the length it decodes to.
	path, _ := encodeMP3(t, "sine=frequency=440:duration=20", "-q:a", "4")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	off := 0
	if string(data[:3]) == "ID3" {
		off = 10 + int(syncsafeToSize(binary.BigEndian.Uint32(data[6:])))
	}
	m, ok := decodeMPA(binary.BigEndian.Uint32(data[off:]))
	if !ok {
		t.Fatal("no first frame")
	}
	end := off + m.frameSize
	i := off + bytes.Index(data[off:end], []byte("Xing"))
	if i < off || binary.BigEndian.Uint32(data[i+4:])&1 == 0 {
		t.Fatal("no Xing frame count to take out")
	}
	binary.BigEndian.PutUint32(data[i+4:], binary.BigEndian.Uint32(data[i+4:])&^1)
	copy(data[i+8:end], data[i+12:end])
	clear(data[end-4 : end])
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	decoded := decodedSeconds(t, path)
	md, err := Extract(path, "")
	if err != nil || math.Abs(md.Duration-decoded) > 1e-6 {
		t.Errorf("an encoder's: %v duration %v, want %v", err, md.Duration, decoded)
	}
}

// A Xing header that gives neither frames nor bytes, but an encoder tag with
// its delay, is no header for the length (the frames are counted, the Xing
// frame among them, as the decoder outputs it), yet the decoder still skips
// that delay at the start (start_skip_samples): the count leaves it out too.
func TestXingWithNoFramesNorBytesSkipsTheEncoderDelay(t *testing.T) {
	t.Parallel()
	// A VBR file whose Xing header has frames, bytes, a TOC and an encoder tag.
	path, _ := encodeMP3(t, "sine=frequency=440:duration=20[a];anoisesrc=d=20:a=0.3[n];[a][n]amix", "-q:a", "4")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	off := 0
	if string(data[:3]) == "ID3" {
		off = 10 + int(syncsafeToSize(binary.BigEndian.Uint32(data[6:])))
	}
	m, ok := decodeMPA(binary.BigEndian.Uint32(data[off:]))
	if !ok {
		t.Fatal("no first frame")
	}
	end := off + m.frameSize
	i := off + bytes.Index(data[off:end], []byte("Xing"))
	if i < off {
		t.Fatal("no Xing frame")
	}
	// The flags cleared, and the fields they named taken out, so the encoder
	// tag follows the flags.
	flags, n := binary.BigEndian.Uint32(data[i+4:]), 0
	for bit, size := range map[uint32]int{1: 4, 2: 4, 4: 100, 8: 4} {
		if flags&bit != 0 {
			n += size
		}
	}
	binary.BigEndian.PutUint32(data[i+4:], 0)
	copy(data[i+8:end], data[i+8+n:end])
	clear(data[end-n : end])
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	decoded := decodedSeconds(t, path)
	md, err := Extract(path, "")
	if err != nil || math.Abs(md.Duration-decoded) > 1e-6 {
		t.Errorf("%v duration %v, want %v (the encoder delay skipped)", err, md.Duration, decoded)
	}
}

// A read for the tags alone (WithoutFrameCount: the setup preview's samples,
// a scan's tag backfill) counts no frames and samples none: a header-less MP3
// takes the first frame's bitrate's estimate, the answer a CBR file gets
// anyway, and only the first block is read.
func TestWithoutFrameCountReadsNoFrames(t *testing.T) {
	t.Parallel()
	data := vbrFrames(20000) // ~8 MiB
	p, reason, read := probeBytes(t, WithoutFrameCount(t.Context()), data, maxProbeTime)
	if reason != "" || p.Counted || p.Duration <= 0 {
		t.Fatalf("reason %q answer %+v, want the estimate", reason, p)
	}
	if read > 2*bfBlock {
		t.Errorf("read %d of %d bytes, want the first block only", read, len(data))
	}
	if _, _, read := probeBytes(t, t.Context(), data, maxProbeTime); read < int64(len(data)) {
		t.Errorf("a count read %d of %d bytes, want the whole file", read, len(data))
	}
	cbr := bytes.Repeat(mp3Frame(9), 20000)
	want, _, _ := probeBytes(t, t.Context(), cbr, maxProbeTime)
	if got, _, _ := probeBytes(t, WithoutFrameCount(t.Context()), cbr, maxProbeTime); got == nil || got.Duration != want.Duration {
		t.Errorf("a CBR file for the tags alone: %+v, want %v as sampled", got, want.Duration)
	}
}

// A header-less MP3 whose sampled frames share one bitrate keeps the
// constant bitrate's estimate (right for true CBR), with no count: only the
// frames sampled are read, not the file. One at 32 kbit/s, the lowest, is
// walked frame by frame for its first mp3LowRateWalk bytes only, never read
// whole (~14 MB an hour).
func TestHeaderlessCBRIsNotCounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		bi      byte
		kbps    int64
		frames  int
		maxRead int64
	}{
		{"128 kbit/s", 9, 128, 20000, 2 << 20},              // 8 MiB
		{"32 kbit/s", 1, 32, 80000, mp3LowRateWalk + 1<<20}, // 8 MiB, ~35 min
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := bytes.Repeat(mp3Frame(tc.bi), tc.frames)
			p, reason, read := probeBytes(t, t.Context(), data, maxProbeTime)
			if reason != "" || p.Counted {
				t.Fatalf("reason %q counted %v, want the estimate", reason, p != nil && p.Counted)
			}
			if want := printTime(rescale(int64(len(data)), 8*14112000, tc.kbps*1000), 1, 14112000); math.Abs(p.Duration-want) > 1e-5 {
				t.Errorf("duration %v, want the bitrate's %v", p.Duration, want)
			}
			if read > tc.maxRead {
				t.Errorf("read %d of %d bytes, want at most %d", read, len(data), tc.maxRead)
			}
		})
	}
}

// A header-less CBR MP3 with a large trailing APEv2 tag (a cover) is sampled
// over its audio, not into the tag, where no frame is found: it keeps the
// bitrate's estimate rather than being counted whole.
func TestHeaderlessCBRWithATrailingTagIsNotCounted(t *testing.T) {
	t.Parallel()
	stream := bytes.Repeat(mp3Frame(9), 5000) // ~2 MB
	items := bytes.Repeat([]byte{0x41}, len(stream)/10)
	apeHead := func(header bool) []byte {
		f := make([]byte, 32)
		copy(f, "APETAGEX")
		binary.LittleEndian.PutUint32(f[8:], 2000)
		binary.LittleEndian.PutUint32(f[12:], uint32(len(items)+32))
		flags := uint32(1 << 31) // it has a header
		if header {
			flags |= 1 << 29
		}
		binary.LittleEndian.PutUint32(f[20:], flags)
		return f
	}
	data := slices.Concat(stream, apeHead(true), items, apeHead(false))
	p, reason, read := probeBytes(t, t.Context(), data, maxProbeTime)
	if reason != "" || p.Counted {
		t.Fatalf("reason %q counted %v, want the estimate", reason, p != nil && p.Counted)
	}
	if read >= int64(len(stream)) {
		t.Errorf("read %d of %d bytes, want the sampled frames only", read, len(data))
	}
}

// A read for the tags alone whose probe runs out of time is not uncounted:
// read again alone it would count nothing either.
func TestWithoutFrameCountIsNeverUncounted(t *testing.T) {
	t.Parallel()
	data := vbrFrames(3000)
	b := newBlockFile(bytes.NewReader(data), int64(len(data)))
	b.ff = ff8
	if _, reason, uncounted := fastProbe(WithoutFrameCount(t.Context()), b, "mp3", time.Nanosecond); !timedOutReason(reason) || uncounted {
		t.Errorf("reason %q uncounted %t, want out of time and not uncounted", reason, uncounted)
	}
	b = newBlockFile(bytes.NewReader(data), int64(len(data)))
	b.ff = ff8
	if _, reason, uncounted := fastProbe(t.Context(), b, "mp3", time.Nanosecond); !timedOutReason(reason) || !uncounted {
		t.Errorf("a read for the length: reason %q uncounted %t, want out of time and uncounted", reason, uncounted)
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
			p, reason, uncounted := fastProbe(ctx, b, "mp3", tc.timeout)
			if p != nil || !strings.HasPrefix(reason, reasonUncounted+"; frame count: limit: ") {
				t.Fatalf("reason %q, want the count cut short", reason)
			}
			if timedOutReason(reason) != tc.timedOut || countTimedOut(reason) != tc.timedOut {
				t.Errorf("reason %q: timed out %t, want %t", reason, !tc.timedOut, tc.timedOut)
			}
			if !uncounted {
				t.Error("not marked uncounted: the scan would stamp the estimate as counted")
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("gave up after %v", took)
			}
		})
	}
}

// An MP3 whose probe runs out of time before it can tell whether the file is
// a header-less VBR one is marked uncounted all the same: ffprobe's estimate
// answers for it, and the scan reads it again rather than stamp the estimate.
func TestProbeOutOfTimeIsUncounted(t *testing.T) {
	t.Parallel()
	data := vbrFrames(1000)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	b := newBlockFile(hangBetween{r: bytes.NewReader(data), from: 0, to: int64(len(data)), hang: hang}, int64(len(data)))
	b.ff = ff8
	p, reason, uncounted := fastProbe(t.Context(), b, "mp3", 100*time.Millisecond)
	if p != nil || !timedOutReason(reason) {
		t.Fatalf("answer %+v, reason %q; want the probe out of time", p, reason)
	}
	if !uncounted {
		t.Error("not marked uncounted: the scan would stamp ffprobe's estimate as counted")
	}
	if countTimedOut(reason) {
		t.Error("the probe out of time read as a count out of time: the scan would keep ffprobe's estimate")
	}
}

// failWindows is a file whose reads of more than a block fail from an offset
// on (a frame count's windows: the probe's own reads, a block at a time,
// answer), giving the bytes before it, and how many reads failed.
type failWindows struct {
	r     io.ReaderAt
	from  int64
	fails atomic.Int32
}

func (f *failWindows) ReadAt(p []byte, off int64) (int, error) {
	if len(p) <= bfBlock || off+int64(len(p)) <= f.from {
		return f.r.ReadAt(p, off)
	}
	f.fails.Add(1)
	n := 0
	if off < f.from {
		n, _ = f.r.ReadAt(p[:f.from-off], off)
	}
	return n, errors.New("input/output error")
}

// A read that fails part way through a count ends it there, the error the
// reason: the count does not go on to look for a frame a byte at a time in
// what it could not read, asking for the failed read again each time.
func TestCountStopsAtAReadError(t *testing.T) {
	t.Parallel()
	data := vbrFrames(20000) // ~8 MiB
	f := &failWindows{r: bytes.NewReader(data), from: 3 << 20}
	b := newBlockFile(f, int64(len(data)))
	b.ff = ff8
	p, reason, uncounted := fastProbe(t.Context(), b, "mp3", maxProbeTime)
	if p != nil || reason != "read: input/output error" {
		t.Fatalf("answer %+v, reason %q; want the read error", p, reason)
	}
	if n := f.fails.Load(); n != 1 {
		t.Errorf("%d reads failed, want the count ended at the first", n)
	}
	if !uncounted || countTimedOut(reason) {
		t.Errorf("uncounted %t, count out of time %t: want uncounted for now, the scan reading it again", uncounted, countTimedOut(reason))
	}
}

// encodeHeaderless writes seconds of a tone under noise as a VBR MP3 with no
// Xing header (ffmpeg -write_xing 0, libmp3lame -q:a 4), and returns its path
// and the length ffmpeg decodes it to; skipped without ffmpeg and libmp3lame.
func encodeHeaderless(t *testing.T, seconds int) (string, float64) {
	t.Helper()
	src := "sine=frequency=440:duration=" + strconv.Itoa(seconds) + "[a];anoisesrc=d=" + strconv.Itoa(seconds) + ":a=0.3[n];[a][n]amix"
	return encodeMP3(t, src, "-q:a", "4", "-write_xing", "0")
}

// encodeMP3 writes the filter graph src as an MP3 at 44.1 kHz (libmp3lame,
// with args), and returns its path and the length ffmpeg decodes it to;
// skipped without ffmpeg and libmp3lame.
func encodeMP3(t *testing.T, src string, args ...string) (string, float64) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	path := filepath.Join(t.TempDir(), "enc.mp3")
	cmd := append([]string{"-v", "error", "-filter_complex", src, "-ar", "44100", "-c:a", "libmp3lame"}, args...)
	out, err := exec.CommandContext(t.Context(), ffmpeg, append(cmd, path)...).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg can't write the fixture (libmp3lame?): %v %s", err, out)
	}
	return path, decodedSeconds(t, path)
}

// decodedSeconds is the length ffmpeg decodes the MP3 at path to (its samples
// over 44.1 kHz).
func decodedSeconds(t *testing.T, path string) float64 {
	t.Helper()
	pcm, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-f", "s16le", "-ac", "1", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	return float64(len(pcm)/2) / 44100
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
	// One that ffprobe fails on keeps the count, the probe's answer standing
	// for ffprobe's (server#452), never none.
	failing, _ := fakeFFprobe(t, "6.1.1", true)
	md, err = Extract(path, failing)
	if err != nil || md.ProbeErr != nil || math.Abs(md.Duration-decoded) > frame || md.Codec != "mp3" {
		t.Errorf("an unknown ffprobe failing: %v %v duration %v codec %q, want %v counted", err, md.ProbeErr, md.Duration, md.Codec, decoded)
	}
	md, _ = Extract(fixture("lame-cbr-noxing.mp3"), failing)
	if md.ProbeErr == nil {
		t.Error("header-less CBR with an unknown ffprobe failing: no ProbeErr, want ffprobe's")
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
		p, reason, _ := fastProbe(bm.Context(), b, "mp3", ProbeTimeout)
		if reason != "" || !p.Counted {
			bm.Fatalf("%s: reason %q, counted %t; want a header-less VBR MP3 counted", path, reason, p != nil && p.Counted)
		}
	}
}

// With an ffprobe whose release the probe has no rules for, a read for the
// tags alone (WithoutFrameCount) of a header-less VBR MP3 that ffprobe fails
// on counts its frames after all, as the read for its length had the count
// stand in: no ProbeErr where the book's index had none (a scan's tag
// backfill would otherwise wait on it and remember the file as rejected,
// server#318). A CBR file that ffprobe fails on keeps its error, as its index
// did, and one ffprobe answers for is not counted.
func TestTagsAloneCountWhenAnUnknownFFprobeFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vbr, cbr := filepath.Join(dir, "vbr.mp3"), filepath.Join(dir, "cbr.mp3")
	if err := os.WriteFile(vbr, vbrFrames(3000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cbr, bytes.Repeat(mp3Frame(9), 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithoutFrameCount(t.Context())
	failing, _ := fakeFFprobe(t, "6.1.1", true)
	md, err := ExtractTimeout(ctx, dir, vbr, failing, ProbeTimeout)
	if want := framesSeconds(3000); err != nil || md.ProbeErr != nil || math.Abs(md.Duration-want) > 1e-6 {
		t.Errorf("header-less VBR, ffprobe failing: %v %v duration %v, want %v counted", err, md.ProbeErr, md.Duration, want)
	}
	if md, _ := ExtractTimeout(ctx, dir, cbr, failing, ProbeTimeout); md.ProbeErr == nil {
		t.Errorf("header-less CBR, ffprobe failing: duration %v, no ProbeErr; want ffprobe's", md.Duration)
	}
	answering, _ := fakeFFprobe(t, "6.1.1", false)
	if md, _ := ExtractTimeout(ctx, dir, vbr, answering, ProbeTimeout); md.ProbeErr != nil || md.Duration != 1 {
		t.Errorf("header-less VBR, ffprobe answering: %v duration %v, want ffprobe's, uncounted", md.ProbeErr, md.Duration)
	}
}

// A picture read whole (dhowden's APIC frame) is not read from the file again
// when the probe reads the whole tag around it: only the tag's other bytes are,
// and the tag comes back as it is on disk (server#269).
func TestBlockFileKeepsTheLastLargeRead(t *testing.T) {
	t.Parallel()
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	r := &countingReader{r: bytes.NewReader(data)}
	b := newBlockFile(r, int64(len(data)))
	pic := make([]byte, 1<<20) // the picture, inside the tag
	const picAt = 100 << 10
	if n, err := b.ReadAt(pic, picAt); n != len(pic) || err != nil {
		t.Fatalf("picture read %d, %v", n, err)
	}
	before := r.n.Load()
	tag := make([]byte, picAt+len(pic)+300<<10) // the whole tag: before, the picture, after
	if n, err := b.ReadAt(tag, 0); n != len(tag) || err != nil {
		t.Fatalf("tag read %d, %v", n, err)
	}
	if !bytes.Equal(tag, data[:len(tag)]) {
		t.Fatal("the tag read back is not the file's bytes")
	}
	if read := r.n.Load() - before; read >= int64(len(pic)) || read < int64(len(tag)-len(pic)) {
		t.Fatalf("the tag read %d bytes from the file, want its %d outside the picture (to a block)", read, len(tag)-len(pic))
	}

	// One past the file's end is short, as before.
	end := make([]byte, 400<<10)
	if n, err := b.ReadAt(end, int64(len(data))-200<<10); n != 200<<10 || err == nil {
		t.Fatalf("a read past the end = %d, %v; want what is there and an error", n, err)
	}
}
