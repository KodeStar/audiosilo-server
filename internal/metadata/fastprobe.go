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
// ffprobe, probe_live_test.go). Its one exception is the length of a VBR MP3
// with no frame count in a Xing or VBRI header: counted, never ffprobe's
// estimate (SRV-16, server#452).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/mounts"
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

// noFFprobeRules is the rules the probe answers by with no ffprobe configured
// (SCAN-SPIKE decision 2, server#269): the Docker image's release, the newest
// known. No ffprobe means no fallback: a file the probe is unsure of has no
// duration, as every file had before.
const noFFprobeRules = ff8

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

// probeLimit is the panic a blockFile's tick raises once a bound is passed
// (and rescale for a time it can't hold): fastProbe recovers it, from wherever
// the reader was.
type probeLimit string

// errProbeDeadline is the probe's own timeout's cause, and errReadDeadline the
// tag read's (the caller's ending context keeps its own).
var (
	errProbeDeadline = errors.New("probe deadline")
	errReadDeadline  = errors.New("read deadline")
)

// timedOutReason reports whether a reason readFile gives is a deadline of its
// own (the probe's or the tag read's), not a bound on bytes, a shape it does
// not model or the caller's context ending.
func timedOutReason(reason string) bool {
	return strings.HasSuffix(reason, "limit: "+errProbeDeadline.Error()) ||
		strings.HasSuffix(reason, "limit: "+errReadDeadline.Error())
}

// fastProbe answers for the file's format as b.ff's ffprobe would, or says why
// it can't ("" when it answered), within the probe's bounds: timeout (capped at
// maxProbeTime) and ctx, and maxProbeBytes. A bound passed, a reader's panic, a
// read error and an answer with no duration are all reasons.
//
// The one answer that is not ffprobe's is a VBR MP3's with no frame count in
// a Xing or VBRI header: ffprobe estimates its length from a bitrate (up to
// ~7% off, SRV-16; 43% for one mostly silence, server#452), so its frames are
// counted instead (mp3Uncounted.count, with bounds of its own: the whole file
// is read), and the result is Counted; under WithoutFrameCount the estimate
// answers, not Counted. One whose count may yet succeed (out of time, the
// caller's context ended, a read that failed) is uncounted, as is an MP3
// whose probe ran out of time before it could tell: the scan reads it again.
// A count that gave up (over an eighth of the audio no frame) or found no
// frames reads the same every time, so it is not.
func fastProbe(ctx context.Context, b *blockFile, format string, timeout time.Duration) (p *probeResult, reason string, uncounted bool) {
	pctx, cancel := context.WithTimeoutCause(ctx, min(timeout, maxProbeTime), errProbeDeadline)
	defer cancel()
	var err error
	if why := b.bounded(pctx, func() {
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
	}); why != "" {
		// A read for the tags alone (WithoutFrameCount) wants no count, so it is
		// never uncounted: read again alone, it would count nothing either.
		return nil, why, format == "mp3" && frameCountWanted(ctx) && (timedOutReason(why) || ctx.Err() != nil)
	}
	uc, headerless := errors.AsType[*mp3Uncounted](err)
	if headerless && b.err == nil {
		var why string
		if p, why = uc.count(ctx, b, timeout); why != "" {
			// The deadline's own words last, so a count out of time reads as
			// the probe's (timedOutReason): with no ffprobe, read again alone.
			err = unsure(uc.Error() + "; frame count: " + why)
		} else {
			err = nil
		}
	}
	p, reason = probeAnswer(b, p, err)
	return p, reason, headerless && reason != "" && (b.err != nil || timedOutReason(reason) || ctx.Err() != nil)
}

// countTimedOut reports whether reason, fastProbe's, is a frame count's own
// deadline (mp3Uncounted.count out of time), not the probe's before it could
// tell, a read error or a bound on bytes.
func countTimedOut(reason string) bool {
	return strings.HasPrefix(reason, reasonUncounted+"; frame count: ") && timedOutReason(reason)
}

// noFrameCount is the context key WithoutFrameCount sets.
type noFrameCount struct{}

// WithoutFrameCount is ctx for a read that wants a file's tags, not its
// length (the setup preview's samples, a scan's backfill of tags into books it
// found unchanged): a VBR MP3 with no frame count in a header then takes the
// first frame's bitrate's estimate rather than having its frames counted,
// which reads the whole file (server#452). The length so read is not to be
// stored: it is not Counted, and may be off. With the probe off for the
// configured ffprobe, an MP3 that ffprobe fails on is counted all the same
// (ExtractTimeout), as a read for its length would have the count stand in.
func WithoutFrameCount(ctx context.Context) context.Context {
	return context.WithValue(ctx, noFrameCount{}, true)
}

// frameCountWanted reports whether ctx is not WithoutFrameCount's.
func frameCountWanted(ctx context.Context) bool { return ctx.Value(noFrameCount{}) == nil }

// probeAnswer is fastProbe's answer from a reader's: p, or why there is none.
func probeAnswer(b *blockFile, p *probeResult, err error) (*probeResult, string) {
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

// bounded runs fn with b armed with a read's bounds: ctx (its deadline the
// read's) and maxProbeBytes. It returns why fn was cut short ("" when it
// wasn't): a bound passed (probeLimit) or a panic, which it recovers, from
// wherever fn was (dhowden's tag read included).
func (b *blockFile) bounded(ctx context.Context, fn func()) (why string) {
	return b.boundedBy(ctx, maxProbeBytes, fn)
}

// boundedBy is bounded with budget bytes to read in place of maxProbeBytes
// (an MP3's frame count reads the whole file).
func (b *blockFile) boundedBy(ctx context.Context, budget int64, fn func()) (why string) {
	b.arm(ctx, budget)
	defer b.disarm()
	defer func() {
		if r := recover(); r != nil {
			if l, ok := r.(probeLimit); ok {
				why = string(l)
				return
			}
			why = fmt.Sprintf("panic: %v", r)
		}
	}()
	fn()
	return ""
}

// ExactDuration is the in-process probe's duration in seconds of the file r
// holds (size bytes; format its extension's, "mp3", "m4a"), by FFmpeg 8.1's
// rules (gapless MP3), and false when the probe is unsure of the file: a shape
// it does not model, a read error or a bound passed (a VBR MP3 with no frame
// count in a Xing or VBRI header whose frames can't all be counted in time
// among them). A
// duration it gives is the container's own (a frame count, a track's length),
// a constant bitrate's (checked across the file) or the frames counted, not a
// VBR guess, so one file can be held to another's (a watch rendition to its
// source).
func ExactDuration(ctx context.Context, r io.ReaderAt, size int64, format string) (float64, bool) {
	b := newNamedBlockFile(r, size)
	b.ff = ff8
	p, reason, _ := fastProbe(ctx, b, strings.ToLower(format), maxProbeTime)
	if reason != "" {
		return 0, false
	}
	return p.Duration, true
}

// ---------------------------------------------------------------------------
// One open, bounded reads: a block cache over the file.

// bfBlock is the block size, and maxBlocks how many blocks one file keeps (4
// MiB: a file's probe reads a few hundred KiB, an M4B's sample tables at most a
// few MiB, and reads of more than four blocks go straight to the file).
const (
	bfBlock   = 64 << 10
	maxBlocks = 64
	// maxBig bounds the large read a blockFile keeps (big): dhowden reads a
	// picture of up to 10 MiB whole, a larger one through the blocks.
	maxBig = 16 << 20
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

	// big is dhowden's last read of more than four blocks (a blockReader that
	// keeps: an MP3's probe follows), a copy, from bigOff: an ID3v2 tag's picture is
	// read whole by dhowden, then again inside the whole tag the probe reads,
	// and a cover of MiBs is not read from the file twice. The probe's own
	// large reads are not kept (nothing reads them again), nor a read of the
	// tags alone, and big is let go once a read has used it.
	big    []byte
	bigOff int64

	stuck  *stuckReads     // the file's mount's reads given up on (waitRead)
	ctx    context.Context // the probe's, its deadline included; nil when not armed
	budget int64           // bytes the probe may still read from the file
	ticks  uint32          // tick's calls, so ctx is looked at every 256th
}

func newBlockFile(f io.ReaderAt, size int64) *blockFile {
	return &blockFile{f: f, size: size, blocks: map[int64][]byte{}, stuck: unknownMount}
}

// arm starts the probe's bounds: ctx (its deadline the probe's) and budget
// bytes.
func (b *blockFile) arm(ctx context.Context, budget int64) {
	b.ctx, b.budget, b.ticks = ctx, budget, 0
}

// disarm ends them (a later read, of tags say, is unbounded as before).
func (b *blockFile) disarm() { b.ctx = nil }

// tick checks the probe's bounds: every read calls it, and so does a loop
// that can go round without reading. Past one it panics with a probeLimit,
// which fastProbe recovers. Cheap: ctx is looked at on every 256th call (and
// by check, on every read from the file and before a costly step).
func (b *blockFile) tick() {
	if b.ctx == nil {
		return
	}
	if b.ticks++; b.ticks&255 == 0 {
		b.check()
	}
}

// check is tick's look at ctx, now: a step that costs far more than a tick
// (a read from the file, a frame to inflate) calls it directly.
func (b *blockFile) check() {
	if b.ctx != nil && b.ctx.Err() != nil {
		panic(probeLimit("limit: " + context.Cause(b.ctx).Error()))
	}
}

// raw reads p at off from the file, noting a failure (a short read ending
// before the file's size included: it shrank since its stat). While armed it
// spends the byte budget and checks ctx, and waits for the read only as long as
// ctx lives (waitRead).
func (b *blockFile) raw(p []byte, off int64) (int, error) {
	var n int
	var err error
	if b.ctx != nil {
		if b.budget -= int64(len(p)); b.budget < 0 {
			panic(probeLimit("limit: probe read budget"))
		}
		b.check()
		n, err = b.waitRead(p, off)
	} else {
		n, err = b.f.ReadAt(p, off)
	}
	if n < len(p) && off+int64(n) < b.size && b.err == nil {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		b.err = err
	}
	return n, err
}

// waitRead is the read of p at off on a goroutine of its own, waited for until
// b's ctx ends: a pread on a network share that stops answering (NFS hard
// mounts, SMB) can't be interrupted, and would hold a scan's reader for good.
// Past ctx it panics with a probeLimit, leaving the read to finish (or not) on
// its own goroutine, counted in its mount's stuckReads until it does (the file
// stays open as long: its descriptor is closed only once the read returns).
// Every caller's buffer is its own, made for this read on the stack the panic
// unwinds (a block, bytesAt's, dhowden's) or dropped with it (a frame count's
// window, kept from read to read: the panic ends the count), so nothing reads
// p after that.
func (b *blockFile) waitRead(p []byte, off int64) (int, error) {
	type res struct {
		n   int
		err error
	}
	// The file alone, not b: a read given up on keeps its goroutine, and that
	// must not keep the blocks and big alive with it.
	f := b.f
	r, ok := await(b.ctx, b.stuck, func() res {
		n, err := f.ReadAt(p, off)
		return res{n, err}
	}, nil)
	if !ok {
		panic(probeLimit("limit: " + context.Cause(b.ctx).Error()))
	}
	return r.n, r.err
}

// maxAbandonedReads is how many reads await may have given up on (an open,
// a pread, an ffprobe run) that have not returned yet, on one mount (mounts.Of), before a new read from a file
// there is refused (audio reads already under way may still be given up on, so
// the count can pass it by the reads running at once; Bounded calls add at
// most the cap to it, as only that many are made at once: stuckReads.calls).
// Each holds a goroutine and
// its file open, so a share that stays hung would otherwise leak a descriptor
// per attempt (server#269): past it a read there fails fast, the file not
// opened (openBlockFile, errReadRefused), until an abandoned read returns. Per
// mount, so one hung share stops reads only from itself, never from every
// library (server#360): the leak stays bounded by the cap times the mounts.
const maxAbandonedReads = 16

// stuckReads counts one mount's reads await gave up on that have not
// returned: one filesystem's, wherever it is mounted (mounts.Of's key), so a
// share mounted anew starts at none and two bind mounts of one share count
// together.
type stuckReads struct {
	mount string // its mount point, as the first file read from it found it (the log's)
	n     atomic.Int32
	// calls holds a slot per Bounded call on the mount from before it is made
	// until it returns, given up on or not: at most maxAbandonedReads at once,
	// so a burst of calls on a share that just stopped answering (a page of
	// covers) can't all be made and pass the cap together before the first is
	// given up on. Per mount, so a hung share's calls hold only its own slots.
	calls chan struct{}
	// logged: the cap's hit was logged since n was last 0.
	logged atomic.Bool
}

// stuckByMount is the stuckReads of each mount a file was read from (its
// lookup a test's stand-in, SetMount): one per mount, never removed.
var stuckByMount = mounts.NewPerMount(func(point string) *stuckReads {
	return &stuckReads{mount: point, calls: make(chan struct{}, maxAbandonedReads)}
})

// unknownMount counts the abandoned reads of a blockFile not read from a named
// file (newBlockFile's default: ExactDuration's of a reader with no Name, a
// test's); nothing checks it against the cap, as no read from a named file is
// refused by it.
var unknownMount = &stuckReads{}

// release uncounts an abandoned read that returned (or one that returned
// before it was marked); back at none, the next hit of the cap is logged
// again.
func (s *stuckReads) release() {
	if s.n.Add(-1) == 0 {
		s.logged.Store(false)
	}
}

// saturated reports whether maxAbandonedReads reads from s's mount are
// abandoned and still running, so a new read there would most likely hang
// too; the first time since none were, it says so in the log.
func (s *stuckReads) saturated() bool {
	if s.n.Load() < maxAbandonedReads {
		return false
	}
	if s.logged.CompareAndSwap(false, true) {
		probeLogger().Warn("audio files: reads stopped answering; skipping reads from this mount until they return",
			"mount", s.mount, "abandoned_reads", s.n.Load())
	}
	return true
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
// to the file, but for what it shares with dhowden's last one (big).
func (b *blockFile) ReadAt(p []byte, off int64) (int, error) { return b.readAt(p, off, false) }

// readAt is ReadAt; keep makes a large read the one kept (big): dhowden's,
// through a blockReader whose (MP3's) probe follows.
func (b *blockFile) readAt(p []byte, off int64, keep bool) (int, error) {
	b.tick()
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= b.size {
		return 0, io.EOF
	}
	if len(p) > 4*bfBlock {
		if lo, hi := max(off, b.bigOff), min(off+int64(len(p)), b.bigOff+int64(len(b.big))); lo < hi {
			return b.aroundBig(p, off, lo, hi)
		}
		n, err := b.raw(p, off)
		if n == len(p) {
			err = nil
			if keep && len(p) <= maxBig {
				b.big, b.bigOff = bytes.Clone(p), off
			}
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

// aroundBig is ReadAt of p at off whose bytes lo to hi are big's: those are
// copied from it, and big let go, and only the rest is read (through the
// blocks, or the file).
func (b *blockFile) aroundBig(p []byte, off, lo, hi int64) (int, error) {
	copy(p[lo-off:hi-off], b.big[lo-b.bigOff:hi-b.bigOff])
	b.big = nil
	if head := p[:lo-off]; len(head) > 0 {
		if n, err := b.ReadAt(head, off); n < len(head) {
			return n, err
		}
	}
	n := int(hi - off)
	if tail := p[hi-off:]; len(tail) > 0 {
		m, err := b.ReadAt(tail, hi)
		if n += m; m < len(tail) {
			return n, err
		}
	}
	return n, nil
}

// bytesAt is the n bytes at off; ok false when they run past the end.
func (b *blockFile) bytesAt(off int64, n int) ([]byte, bool) {
	if off < 0 || n < 0 || off+int64(n) > b.size {
		return nil, false
	}
	// A read raw would refuse for the budget is refused before its buffer is
	// made: a size field up to the file's (an MKV's EBML header) is no
	// allocation of the whole file.
	if b.ctx != nil && n > 4*bfBlock && int64(n) > b.budget {
		panic(probeLimit("limit: probe read budget"))
	}
	buf := make([]byte, n)
	m, _ := b.ReadAt(buf, off)
	return buf, m == n
}

// blockReader is a ReadSeeker over a blockFile, for dhowden. keep makes its
// large reads the one the blockFile keeps (big): only when an MP3's probe follows
// that reads them again, never for a read of the tags alone.
type blockReader struct {
	b    *blockFile
	pos  int64
	keep bool
}

func (r *blockReader) Read(p []byte) (int, error) {
	if r.pos >= r.b.size {
		return 0, io.EOF
	}
	n, err := r.b.readAt(p, r.pos, r.keep)
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
	if !x.IsInt64() {
		// FFmpeg gives INT64_MIN here, or wraps on its fast path: a crafted
		// file's, which ffprobe reads instead (server#269).
		panic(probeLimit("a time past int64"))
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
	c    []*avChapter
	byID map[int64]*avChapter // each id's chapter (ids are unique in c)
}

// add is avpriv_new_chapter: nil (no chapter) when it ends before it starts.
// An id already added is that chapter, updated (FFmpeg's search, skipped while
// the ids rise, finds the same one).
func (l *chapterList) add(id, num, den, start, end int64, hasEnd bool, title string) *avChapter {
	if hasEnd && start > end {
		return nil
	}
	ch := l.byID[id]
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

// newNamedBlockFile is newBlockFile for a request's read of r: a named file's
// reads given up on count on its mount (a rendition's source on a share that
// stopped answering), as a scan's do.
func newNamedBlockFile(r io.ReaderAt, size int64) *blockFile {
	b := newBlockFile(r, size)
	if f, ok := r.(interface{ Name() string }); ok {
		b.stuck = stuckByMount.For(f.Name())
	}
	return b
}
