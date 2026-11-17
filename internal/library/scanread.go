package library

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// readSlots bounds how many audio files a scan reads at once (tags, then
// ffprobe): one slot per read under way. An on-demand index reads with one.
type readSlots struct {
	ch chan struct{}
	// alone is read-held by every read under way and held by a read retried on
	// its own (extract): a retry waiting for it holds back the reads that would
	// start after it, so it waits only for those already under way.
	alone sync.RWMutex
	// active is how many reads are under way, started how many have begun: a
	// read overlapped another when either moved while it ran.
	active  atomic.Int64
	started atomic.Uint64
	// timeout is each ffprobe run's (Scanner.probeTimeout).
	timeout time.Duration
	// readFile is metadata.ExtractTimeout, or a test's stand-in for it.
	readFile func(ctx context.Context, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata
}

// newReadSlots returns n slots (at least one) whose ffprobe runs get timeout.
func newReadSlots(n int, timeout time.Duration) *readSlots {
	return &readSlots{ch: make(chan struct{}, max(n, 1)), timeout: timeout, readFile: extractFile}
}

// extractFile is metadata.ExtractTimeout's reading (it never errs).
func extractFile(ctx context.Context, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata {
	md, _ := metadata.ExtractTimeout(ctx, absPath, ffprobePath, timeout)
	return md
}

// size is how many files may be read at once.
func (r *readSlots) size() int { return cap(r.ch) }

// extract is metadata.ExtractTimeout holding a slot while it reads; nil when
// ctx ends while it waits for one (stopped: the scan drops the reading).
//
// A read whose ffprobe ran out of time while others were reading is read again
// on its own: files sharing a slow disk or a busy share can each take longer
// than ffprobe is given, and a healthy file must not be recorded as a read
// problem because others were read beside it. Only a time-out alone is the
// file's problem. So is one whose in-process read ran out of time with no
// ffprobe to fall back to (ProbeTimedOut): it would otherwise have no duration.
// And one whose header-less VBR MP3 went uncounted (LengthUncounted): its
// length would otherwise be ffprobe's estimate until a later scan.
func (r *readSlots) extract(ctx context.Context, absPath, ffprobePath string) *metadata.Metadata {
	md, shared := r.read(ctx, absPath, ffprobePath)
	if !shared || !(lengthTimedOut(md) || md.LengthUncounted()) {
		return md
	}
	// One retry at a time, each waiting for the reads under way to finish.
	r.alone.Lock()
	defer r.alone.Unlock()
	if ctx.Err() != nil {
		return md // stopped: dropped by the scan
	}
	return r.readFile(ctx, absPath, ffprobePath, r.timeout)
}

// lengthTimedOut reports whether md's length read ran out of time: its ffprobe
// run (metadata.ErrProbeTimeout) or, with no ffprobe, the in-process read
// (ProbeTimedOut).
func lengthTimedOut(md *metadata.Metadata) bool {
	return md != nil && (errors.Is(md.ProbeErr, metadata.ErrProbeTimeout) || md.ProbeTimedOut())
}

// readCutShort reports whether md's read leaves the file unknown, or its
// length: cut short or refused (TagsUnread), or its length unread
// (lengthUnread). A book with such a file is kept as stored
// (catalog.Book.ReadCutShort), never rewritten without that file's length; one
// whose files' tags were all read is kept only while its files are the stored
// ones (catalog.Book.LengthOnly).
func readCutShort(md *metadata.Metadata) bool {
	return md.TagsUnread() || lengthUnread(md)
}

// lengthUnread reports whether md has no length for its file that a read
// could give: its length's read out of time even alone (lengthTimedOut: a part
// on a share that stopped answering mid-read; server#360), or a file that is
// no regular file (metadata.ErrNotRegular: a FIFO, a device). The latter is a
// known bad file, not a read cut short: with its book's other files unchanged
// the stored book stays, and a change to another part rewrites it
// (catalog.Book.LengthOnly, BookFile.LengthUnread).
func lengthUnread(md *metadata.Metadata) bool {
	return lengthTimedOut(md) || (md != nil && errors.Is(md.OpenErr, metadata.ErrNotRegular))
}

// read is metadata.ExtractTimeout holding a slot, and whether another read ran
// beside it; nil when ctx ends first.
func (r *readSlots) read(ctx context.Context, absPath, ffprobePath string) (*metadata.Metadata, bool) {
	select {
	case r.ch <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	defer func() { <-r.ch }()
	r.alone.RLock()
	defer r.alone.RUnlock()
	if ctx.Err() != nil { // stopped while a retry ran alone
		return nil, false
	}
	start := r.started.Add(1)
	before := r.active.Add(1)
	md := r.readFile(ctx, absPath, ffprobePath, r.timeout)
	after := r.active.Add(-1)
	return md, before > 1 || after > 0 || r.started.Load() != start
}

// readFiles reads a folder book's files, in their order: the first is first
// (already read, as the book's primary file), the rest are read with slots, as
// many at once as they allow. With room for one at a time they are read on the
// caller's goroutine: an on-demand index reads on leadIndex's, which recovers a
// reader's panic (errIndexAborted) rather than the server going down with it.
func (s *Scanner) readFiles(ctx context.Context, lib catalog.Library, files []catalog.BookFile, first *metadata.Metadata, slots *readSlots) []*metadata.Metadata {
	mds := make([]*metadata.Metadata, len(files))
	if len(files) == 0 {
		return mds
	}
	mds[0] = first
	read := func(i int) {
		if ctx.Err() == nil { // stopped: the scan drops this book's reading
			mds[i] = slots.extract(ctx, absOf(lib, files[i].RelPath), s.ffprobePath)
		}
	}
	workers := min(slots.size(), len(files)-1)
	if workers <= 1 {
		for i := 1; i < len(files); i++ {
			read(i)
		}
		return mds
	}
	rest := make([]int, len(files)-1)
	for i := range rest {
		rest[i] = i + 1
	}
	pool.Each(ctx, workers, rest, func() bool { return true }, func(i *int) { read(*i) })
	return mds
}

// bookRead is a book a scan is reading (enrich, on its own goroutine) and will
// write once done is closed.
type bookRead struct {
	book    *catalog.Book
	existed bool // indexed before this scan
	done    chan struct{}
	// The book's flight (Scanner.startFlight), held from before the read until
	// the book is written or dropped; nil when the scan stopped first.
	flight *indexFlight
	// left is set once the scan stopped and left this read under way: the
	// flight's indexFlight.left.
	left atomic.Bool
}

// endFlight ends the read's flight once its book is written (err nil) or not,
// after the read is done; a book not written leaves its waiters to read it
// themselves.
func (r *bookRead) endFlight(s *Scanner, err error) {
	if r.flight == nil {
		return
	}
	if err != nil {
		err = errReadDropped
	}
	s.endFlight(r.flight, err)
	r.flight = nil
}

// finished reports whether the read is done, without waiting.
func (r *bookRead) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}
