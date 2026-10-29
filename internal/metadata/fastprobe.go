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
	"math"
	"math/big"
	"math/bits"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	e, ok := rulesByPath.Load(ffprobePath)
	if !ok {
		e, _ = rulesByPath.LoadOrStore(ffprobePath, &pathRules{})
	}
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

// The probe's own bounds, so a shape nobody found can't hold a scan's reader
// (ffprobe's timeout covers only the fallback): a file's probe gets at most
// maxProbeTime (or the caller's ffprobe timeout, or its context, whichever ends
// first) and maxProbeBytes read from the file, far above what a real file needs
// (a few hundred KiB; the real library's p95 is under 1 MiB). Either one ends
// the probe, and the file goes to ffprobe.
const (
	maxProbeTime  = 5 * time.Second
	maxProbeBytes = 64 << 20
)

// probeLimit is the panic a blockFile's tick raises once a bound is passed:
// fastProbe recovers it, from wherever the reader was.
type probeLimit string

// fastProbe answers for the file's format as b.ff's ffprobe would, or says why
// it can't ("" when it answered), within the probe's bounds: timeout (capped at
// maxProbeTime) and ctx, and maxProbeBytes. A bound passed, a reader's panic, a
// read error and an answer with no duration are all reasons.
func fastProbe(ctx context.Context, b *blockFile, format string, timeout time.Duration) (p *probeResult, reason string) {
	b.arm(ctx, min(timeout, maxProbeTime), maxProbeBytes)
	defer b.disarm()
	defer func() {
		if r := recover(); r != nil {
			if l, ok := r.(probeLimit); ok {
				p, reason = nil, string(l)
				return
			}
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
	p, reason := fastProbe(context.Background(), b, fileFormat(path), maxProbeTime)
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
// file goes to ffprobe, whose own error is the one recorded. While the probe
// runs it is armed with the probe's bounds, checked by tick.
type blockFile struct {
	f      io.ReaderAt
	size   int64
	blocks map[int64][]byte
	err    error
	ff     ffRules // the rules the probe reads it by

	armed    bool
	ctx      context.Context
	deadline time.Time
	budget   int64  // bytes the probe may still read from the file
	ticks    uint32 // tick's calls, so the clock is read every 256th
}

func newBlockFile(f io.ReaderAt, size int64) *blockFile {
	return &blockFile{f: f, size: size, blocks: map[int64][]byte{}}
}

// arm starts the probe's bounds: timeout from now, ctx, budget bytes.
func (b *blockFile) arm(ctx context.Context, timeout time.Duration, budget int64) {
	b.armed, b.ctx, b.deadline, b.budget, b.ticks = true, ctx, time.Now().Add(timeout), budget, 0
}

// disarm ends them (a later read, of tags say, is unbounded as before).
func (b *blockFile) disarm() { b.armed, b.ctx = false, nil }

// tick checks the probe's bounds: every read calls it, and so does a loop
// that can go round without reading. Past one it panics with a probeLimit,
// which fastProbe recovers. Cheap: the clock and ctx are looked at on every
// 256th call (and on every read from the file, in raw).
func (b *blockFile) tick() {
	if !b.armed {
		return
	}
	if b.ticks++; b.ticks&255 == 0 {
		b.checkTime()
	}
}

// checkTime is tick's look at the clock and ctx.
func (b *blockFile) checkTime() {
	if b.ctx.Err() != nil {
		panic(probeLimit("limit: " + b.ctx.Err().Error()))
	}
	if time.Now().After(b.deadline) {
		panic(probeLimit("limit: probe deadline"))
	}
}

// raw reads p at off from the file, noting a failure (a short read ending
// before the file's size included: it shrank since its stat). While armed it
// spends the byte budget and checks the clock.
func (b *blockFile) raw(p []byte, off int64) (int, error) {
	if b.armed {
		if b.budget -= int64(len(p)); b.budget < 0 {
			panic(probeLimit("limit: probe read budget"))
		}
		b.checkTime()
	}
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
	if len(b.blocks) >= maxBlocks {
		for k := range b.blocks { // full: any block makes room
			delete(b.blocks, k)
			break
		}
	}
	b.blocks[i] = buf
	return buf
}

// ReadAt reads through the block cache; a large read (a picture) goes straight
// to the file.
func (b *blockFile) ReadAt(p []byte, off int64) (int, error) {
	b.tick()
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

// idx finds a key's entry in constant time (keys are unique), so a file with
// thousands of tags is not quadratic as FFmpeg's linear search is.
type avDict struct {
	e   []dictEntry
	idx map[string]int // the key with ASCII folded to lower case -> its entry
}

// foldKey is k with ASCII upper case folded to lower, as av_dict_get matches.
func foldKey(k string) string {
	for i := 0; i < len(k); i++ {
		if 'A' <= k[i] && k[i] <= 'Z' {
			b := []byte(k)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return k
}

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
	if i, ok := d.idx[foldKey(k)]; ok {
		return i
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
		d.removeAt(i)
	}
	if d.idx == nil {
		d.idx = map[string]int{}
	}
	d.idx[foldKey(k)] = len(d.e)
	d.e = append(d.e, dictEntry{k, v})
}

func (d *avDict) del(k string) {
	if i := d.find(k); i >= 0 {
		d.removeAt(i)
	}
}

// removeAt is av_dict_set's removal: the last entry takes the slot.
func (d *avDict) removeAt(i int) {
	last := len(d.e) - 1
	delete(d.idx, foldKey(d.e[i].k))
	if i != last {
		d.e[i] = d.e[last]
		d.idx[foldKey(d.e[i].k)] = i
	}
	d.e = d.e[:last]
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
		out[strings.ToLower(e.k)] = validUTF8(cutNUL(e.v))
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
	if c > 0 {
		// The product and its rounding fit in 64 bits: the same answer
		// without big.Int (finish calls this for every pair of chapters).
		ua, ub := uint64(a), uint64(b)
		neg := (a < 0) != (b < 0)
		if a < 0 {
			ua = -ua
		}
		if b < 0 {
			ub = -ub
		}
		hi, lo := bits.Mul64(ua, ub)
		if half := uint64(c) >> 1; hi == 0 && lo <= math.MaxInt64-half {
			q := int64((lo + half) / uint64(c))
			if neg && a != 0 && b != 0 {
				q = -q
			}
			return q
		}
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
}

type chapterList struct {
	c         []*avChapter
	byID      map[int64]*avChapter // each id's chapter (ids are unique in c)
	monotonic bool
}

// find is the chapter with id; nil for none.
func (l *chapterList) find(id int64) *avChapter { return l.byID[id] }

// add is avpriv_new_chapter: nil (no chapter) when it ends before it starts.
func (l *chapterList) add(id, num, den, start, end int64, hasEnd bool, title string) *avChapter {
	if hasEnd && start > end {
		return nil
	}
	var ch *avChapter
	if len(l.c) == 0 {
		l.monotonic = true
	} else if !l.monotonic || l.c[len(l.c)-1].id >= id {
		if ch = l.find(id); ch == nil {
			l.monotonic = false
		}
	}
	if ch == nil {
		ch = &avChapter{}
		l.c = append(l.c, ch)
		if l.byID == nil {
			l.byID = map[int64]*avChapter{}
		}
		l.byID[id] = ch
	}
	ch.title = title
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
	tt := slices.Clone(l.c)
	slices.SortStableFunc(tt, cmpChapterStart)
	for i, ch := range tt {
		if ch.hasEnd {
			continue
		}
		end := int64(1<<63 - 1)
		if maxTime != 0 {
			end = rescale(maxTime, ch.den, 1000000*ch.num)
		}
		// Only the next chapter in the timetable: one starting with ch leaves
		// ch to the format's end.
		if i+1 < len(tt) {
			ch1 := tt[i+1]
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
			Index: i, Title: validUTF8(cutNUL(ch.title)),
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

// validUTF8 is s as ffprobe's JSON writer leaves it (validate_string): each
// sequence av_utf8_decode rejects, U+FFFE and U+FFFF among them, becomes one
// U+FFFD.
func validUTF8(s string) string {
	if utf8.ValidString(s) && !strings.ContainsAny(s, "\uFFFE\uFFFF") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		n, ok := avUTF8Decode(s[i:])
		if ok {
			b.WriteString(s[i : i+n])
		} else {
			b.WriteString("\uFFFD")
		}
		i += n
	}
	return b.String()
}

// avUTF8Decode is av_utf8_decode with no flags on a non-empty s: the bytes
// the sequence at s[0] takes and whether it is accepted. A bad or missing
// continuation byte takes only the lead byte; an overlong, out-of-range,
// surrogate or non-character code takes the whole sequence.
func avUTF8Decode(s string) (int, bool) {
	code := uint64(s[0])
	if code&0xc0 == 0x80 || code >= 0xfe {
		return 1, false
	}
	top := (code & 128) >> 1
	p, tail := 1, 0
	for code&top != 0 {
		tail++
		if p >= len(s) || s[p]&0xc0 != 0x80 {
			return 1, false
		}
		code = code<<6 + uint64(s[p]&0x3f)
		p++
		top <<= 5
	}
	code &= top<<1 - 1
	minCode := [6]uint64{0, 0x80, 0x800, 0x10000, 0x200000, 0x4000000}
	switch {
	case code < minCode[tail], code > 0x10ffff, code >= 0xd800 && code <= 0xdfff, code == 0xfffe, code == 0xffff:
		return p, false
	}
	return p, true
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
