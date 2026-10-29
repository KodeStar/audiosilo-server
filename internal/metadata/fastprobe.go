package metadata

// The in-process probe (server#261): what ffprobe would answer for a file,
// read in Go, so a scan runs no ffprobe process for most files. Its answer is
// ffprobe's, value for value: the format duration as ffprobe prints it
// ("%f"), the first audio stream's codec, the chapters, and the container tags
// named and ordered as FFmpeg's dictionary keeps them. Each format reader is a
// port of FFmpeg 7.1's demuxer for the parts ffprobe's answer depends on
// (fastprobe_mp3.go, fastprobe_mp4.go, fastprobe_xiph.go, fastprobe_mkv.go),
// with the rules of the configured ffprobe's release where they differ
// (ffRules: FFmpeg 7.1 and 8.1 are known; any other ffprobe reads every file,
// as before). A reader meeting a shape it does not model exactly says why (unsure), and
// the file goes to ffprobe as before: parity is the contract, and the fixture
// corpus in testdata/probe checks it (probe_golden_test.go, and against a live
// ffprobe, probe_live_test.go).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ffRules is the FFmpeg release whose answers the in-process probe gives: the
// configured ffprobe's, so a file reads as that ffprobe read it. A release is
// known once the fixture corpus checks it (testdata/probe/golden-<release>.json);
// for any other (an older one, a newer one, a master build) the probe is off
// and ffprobe reads every file.
type ffRules int

const (
	ffNone ffRules = 0  // an ffprobe whose rules the probe doesn't know
	ff7    ffRules = 71 // FFmpeg 7.1
	ff8    ffRules = 81 // FFmpeg 8.1: MP3 gapless durations, empty UTF-16 ID3 strings
)

// knownRules are the releases the probe answers for, by the version ffprobe
// names (its first line: "ffprobe version 8.1.3 Copyright ...").
var knownRules = map[string]ffRules{"7.1": ff7, "8.1": ff8}

// rulesByPath caches each ffprobe path's rules, read once per process.
var rulesByPath sync.Map // ffprobe path -> *pathRules

type pathRules struct {
	mu    sync.Mutex
	known bool // ff is settled
	ff    ffRules
}

// ffprobeRules is the rules the probe follows for the ffprobe at ffprobePath,
// from its -version (run once per path per process; the first callers wait
// for it); ffNone when it can't be run or names a release the probe doesn't
// know. A caller whose ctx ends first gets ffNone, and the next one asks again.
func ffprobeRules(ctx context.Context, ffprobePath string) ffRules {
	e, _ := rulesByPath.LoadOrStore(ffprobePath, &pathRules{})
	r := e.(*pathRules)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known {
		return r.ff
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, ffprobePath, "-version").Output()
	if err != nil && ctx.Err() != nil {
		return ffNone
	}
	if err == nil {
		r.ff = rulesFor(string(out))
	}
	r.known = true
	return r.ff
}

// rulesFor is the rules for an ffprobe's -version output: its release's
// major.minor ("7.1.1", "7.1.1-3ubuntu1", "n8.1.3" read as 7.1, 7.1, 8.1).
func rulesFor(version string) ffRules {
	line, _, _ := strings.Cut(version, "\n")
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != "ffprobe" || f[1] != "version" {
		return ffNone
	}
	parts := strings.SplitN(strings.TrimPrefix(f[2], "n"), ".", 3)
	if len(parts) < 2 {
		return ffNone
	}
	minor, _, _ := strings.Cut(parts[1], "-")
	return knownRules[parts[0]+"."+minor]
}

// unsure is what a format reader returns for a shape it does not model: the
// file is read by ffprobe instead.
type unsure string

func (u unsure) Error() string { return string(u) }

// fastProbe answers for the file's format as b.ff's ffprobe would, or says why
// it can't ("" when it answered). A reader's panic, a read error and an answer
// with no duration are all reasons.
func fastProbe(b *blockFile, format string) (p *probeResult, reason string) {
	defer func() {
		if r := recover(); r != nil {
			p, reason = nil, fmt.Sprintf("panic: %v", r)
		}
	}()
	var err error
	switch format {
	case "mp3":
		p, err = probeMP3(b)
	case "m4b", "m4a", "mp4":
		p, err = probeMP4(b)
	case "flac":
		p, err = probeFLAC(b)
	case "ogg", "opus":
		p, err = probeOgg(b)
	case "mka":
		p, err = probeMKV(b)
	default:
		err = unsure("format " + format)
	}
	switch {
	case b.err != nil:
		return nil, "read: " + b.err.Error()
	case err != nil:
		var u unsure
		if errors.As(err, &u) {
			return nil, string(u)
		}
		return nil, "malformed: " + err.Error()
	case p.Duration <= 0:
		return nil, "no duration"
	}
	return p, ""
}

// probeFile is fastProbe over the file at path with ff's rules, for the
// comparison with ffprobe (probeview.go).
func probeFile(path string, ff ffRules) (*probeResult, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	b := newBlockFile(f, info.Size())
	b.ff = ff
	p, reason := fastProbe(b, fileFormat(path))
	return p, reason, nil
}

// ---------------------------------------------------------------------------
// One open, bounded reads: a block cache over the file.

// bfBlock is the block size, and maxBlocks how many blocks one file keeps (4
// MiB: a file's probe reads a few hundred KiB, an M4B's sample tables at most a
// few MiB, and reads of more than four blocks go straight to the file).
const (
	bfBlock   = 64 << 10
	maxBlocks = 64
)

// blockFile reads the file in 64 KiB blocks it keeps, so dhowden, the narrator
// walk and the probe read each block once. A failed read (anything but the end
// of the file at its size) is kept in err: the probe is then unsure, and the
// file goes to ffprobe, whose own error is the one recorded.
type blockFile struct {
	f      *os.File
	size   int64
	blocks map[int64][]byte
	err    error
	ff     ffRules // the rules the probe reads it by
}

func newBlockFile(f *os.File, size int64) *blockFile {
	return &blockFile{f: f, size: size, blocks: map[int64][]byte{}}
}

// raw reads p at off from the file, noting a failure (a short read ending
// before the file's size included: it shrank since its stat).
func (b *blockFile) raw(p []byte, off int64) (int, error) {
	n, err := b.f.ReadAt(p, off)
	if n < len(p) && off+int64(n) < b.size && b.err == nil {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		b.err = err
	}
	return n, err
}

func (b *blockFile) block(i int64) []byte {
	if blk, ok := b.blocks[i]; ok {
		return blk
	}
	buf := make([]byte, min(bfBlock, b.size-i*bfBlock))
	n, _ := b.raw(buf, i*bfBlock)
	buf = buf[:n]
	if len(b.blocks) < maxBlocks {
		b.blocks[i] = buf
	}
	return buf
}

// ReadAt reads through the block cache; a large read (a picture) goes straight
// to the file.
func (b *blockFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= b.size {
		return 0, io.EOF
	}
	if len(p) > 4*bfBlock {
		n, err := b.raw(p, off)
		if n == len(p) {
			err = nil
		}
		return n, err
	}
	n := 0
	for n < len(p) && off+int64(n) < b.size {
		at := off + int64(n)
		blk := b.block(at / bfBlock)
		o := at % bfBlock
		if o >= int64(len(blk)) {
			break
		}
		n += copy(p[n:], blk[o:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// bytesAt is the n bytes at off; ok false when they run past the end.
func (b *blockFile) bytesAt(off int64, n int) ([]byte, bool) {
	if off < 0 || n < 0 || off+int64(n) > b.size {
		return nil, false
	}
	buf := make([]byte, n)
	m, _ := b.ReadAt(buf, off)
	return buf, m == n
}

// blockReader is a ReadSeeker over a blockFile, for dhowden.
type blockReader struct {
	b   *blockFile
	pos int64
}

func (r *blockReader) Read(p []byte) (int, error) {
	if r.pos >= r.b.size {
		return 0, io.EOF
	}
	n, err := r.b.ReadAt(p, r.pos)
	r.pos += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

func (r *blockReader) Seek(offset int64, whence int) (int64, error) {
	var at int64
	switch whence {
	case io.SeekStart:
		at = offset
	case io.SeekCurrent:
		at = r.pos + offset
	case io.SeekEnd:
		at = r.b.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if at < 0 {
		return 0, errors.New("seek to a negative position")
	}
	r.pos = at
	return at, nil
}

// ---------------------------------------------------------------------------
// FFmpeg's AVDictionary: ordered, keys matched without ASCII case, a replaced
// entry's slot taken by the last one and the new value appended.

type dictEntry struct{ k, v string }

type avDict struct{ e []dictEntry }

const (
	dictDontOverwrite = 1 << iota
	dictAppend
)

func asciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func (d *avDict) find(k string) int {
	for i, e := range d.e {
		if asciiFold(e.k, k) {
			return i
		}
	}
	return -1
}

func (d *avDict) get(k string) (string, bool) {
	if i := d.find(k); i >= 0 {
		return d.e[i].v, true
	}
	return "", false
}

func (d *avDict) set(k, v string, flags int) {
	if i := d.find(k); i >= 0 {
		if flags&dictDontOverwrite != 0 {
			return
		}
		if flags&dictAppend != 0 {
			v = d.e[i].v + v
		}
		last := len(d.e) - 1
		d.e[i] = d.e[last]
		d.e = d.e[:last]
	}
	d.e = append(d.e, dictEntry{k, v})
}

func (d *avDict) del(k string) {
	if i := d.find(k); i >= 0 {
		last := len(d.e) - 1
		d.e[i] = d.e[last]
		d.e = d.e[:last]
	}
}

// conv is ff_metadata_conv with only a source table.
func (d *avDict) conv(table [][2]string) {
	var nd avDict
	for _, e := range d.e {
		k := e.k
		for _, c := range table {
			if asciiFold(k, c[0]) {
				k = c[1]
				break
			}
		}
		nd.set(k, e.v, 0)
	}
	*d = nd
}

// tags is the dictionary as probe() hands it on: keys lowercased (normalizeTags)
// and values as ffprobe's JSON carries them (C strings, valid UTF-8).
func (d *avDict) tags() map[string]string {
	out := make(map[string]string, len(d.e))
	for _, e := range d.e {
		v := e.v
		if i := strings.IndexByte(v, 0); i >= 0 {
			v = v[:i]
		}
		out[strings.ToLower(e.k)] = string([]rune(v))
	}
	return out
}

// ---------------------------------------------------------------------------
// Timing arithmetic as FFmpeg does it.

// rescale is av_rescale_rnd(a, b, c, AV_ROUND_NEAR_INF).
func rescale(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	cc := big.NewInt(c)
	neg := x.Sign() < 0
	x.Abs(x)
	x.Add(x, new(big.Int).Rsh(cc, 1))
	x.Quo(x, cc)
	if neg {
		x.Neg(x)
	}
	return x.Int64()
}

// tbToUS is av_rescale_q(ts, num/den, 1/1000000).
func tbToUS(ts, num, den int64) int64 { return rescale(ts, num*1000000, den) }

// printTime is ffprobe's "%f" of ts in num/den, read back as probe() reads it.
func printTime(ts, num, den int64) float64 {
	s := strconv.FormatFloat(float64(ts)*(float64(num)/float64(den)), 'f', 6, 64)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// avChapter is an AVChapter: times in num/den, end -1 for none (AV_NOPTS_VALUE).
type avChapter struct {
	id         int64
	num, den   int64
	start, end int64
	hasEnd     bool
	title      string
	hasTitle   bool
}

type chapterList struct {
	c         []*avChapter
	monotonic bool
}

// add is avpriv_new_chapter.
func (l *chapterList) add(id, num, den, start, end int64, hasEnd bool, title string, hasTitle bool) *avChapter {
	var ch *avChapter
	if len(l.c) == 0 {
		l.monotonic = true
	} else if !l.monotonic || l.c[len(l.c)-1].id >= id {
		for _, c := range l.c {
			if c.id == id {
				ch = c
			}
		}
		if ch == nil {
			l.monotonic = false
		}
	}
	if ch == nil {
		ch = &avChapter{}
		l.c = append(l.c, ch)
	}
	ch.title, ch.hasTitle = title, hasTitle
	ch.id, ch.num, ch.den, ch.start, ch.end, ch.hasEnd = id, num, den, start, end, hasEnd
	return ch
}

// finish is compute_chapters_end (format duration and start in microseconds;
// durUS <= 0 for none), then the chapters as probe() reads ffprobe's.
func (l *chapterList) finish(durUS, startUS int64, hasStart bool) []Chapter {
	if len(l.c) == 0 {
		return nil
	}
	var maxTime int64
	if durUS > 0 {
		maxTime = durUS
		if hasStart {
			maxTime += startUS
		}
	}
	// timetable: sorted by start (stable on equal, as qsort of distinct pointers
	// compares only start: FFmpeg's ordering of equal starts is unspecified).
	tt := make([]*avChapter, len(l.c))
	copy(tt, l.c)
	for i := 1; i < len(tt); i++ {
		for j := i; j > 0 && cmpChapterStart(tt[j], tt[j-1]) < 0; j-- {
			tt[j], tt[j-1] = tt[j-1], tt[j]
		}
	}
	for i, ch := range tt {
		if ch.hasEnd {
			continue
		}
		end := int64(1<<63 - 1)
		if maxTime != 0 {
			end = rescale(maxTime, ch.den, 1000000*ch.num)
		}
		for _, ch1 := range tt[i+1:] {
			next := rescale(ch1.start, ch1.num*ch.den, ch1.den*ch.num)
			if next > ch.start && next < end {
				end = next
			}
		}
		if end == int64(1<<63-1) || end < ch.start {
			end = ch.start
		}
		ch.end, ch.hasEnd = end, true
	}
	out := make([]Chapter, 0, len(l.c))
	for i, ch := range l.c {
		out = append(out, Chapter{
			Index: i, Title: string([]rune(cutNUL(ch.title))),
			Start: printTime(ch.start, ch.num, ch.den), End: printTime(ch.end, ch.num, ch.den),
		})
	}
	return out
}

func cmpChapterStart(a, b *avChapter) int {
	x := new(big.Int).Mul(big.NewInt(a.start), big.NewInt(a.num*b.den))
	y := new(big.Int).Mul(big.NewInt(b.start), big.NewInt(b.num*a.den))
	return x.Cmp(y)
}

func cutNUL(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

// result builds the probeResult from a format's findings.
func result(durUS int64, codec string, d *avDict, chaps []Chapter) *probeResult {
	p := &probeResult{Codec: codec, Tags: map[string]string{}, Chapters: chaps}
	if d != nil {
		p.Tags = d.tags()
	}
	if durUS > 0 {
		p.Duration = printTime(durUS, 1, 1000000)
	}
	return p
}
