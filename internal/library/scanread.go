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
	readFile readFunc
}

// newReadSlots returns n slots (at least one) whose ffprobe runs get timeout.
func newReadSlots(n int, timeout time.Duration) *readSlots {
	return &readSlots{ch: make(chan struct{}, max(n, 1)), timeout: timeout, readFile: extractFile}
}

// readSlots is newReadSlots for this scanner's reads: probeTimeout each, through
// its reader.
func (s *Scanner) readSlots(n int) *readSlots {
	slots := newReadSlots(n, s.probeTimeout)
	slots.readFile = s.reader()
	return slots
}

// reader is how this scanner reads an audio file: its readFile when one is set
// (tests), else extractFile.
func (s *Scanner) reader() readFunc {
	if s.readFile != nil {
		return s.readFile
	}
	return extractFile
}

// readFunc reads the audio file at absPath, under the library root root,
// opened through it (metadata.ExtractTimeout), its ffprobe run given timeout.
type readFunc func(ctx context.Context, root, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata

// extractFile is metadata.ExtractTimeout's reading (it never errs).
func extractFile(ctx context.Context, root, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata {
	md, _ := metadata.ExtractTimeout(ctx, root, absPath, ffprobePath, timeout)
	return md
}

// size is how many files may be read at once.
func (r *readSlots) size() int { return cap(r.ch) }

// extract is metadata.ExtractTimeout holding a slot while it reads; nil when
// ctx ends before it is done (stopped: the scan drops the reading, which the
// stop may have cut short).
//
// A read whose ffprobe ran out of time while others were reading is read again
// on its own: files sharing a slow disk or a busy share can each take longer
// than ffprobe is given, and a healthy file must not be recorded as a read
// problem because others were read beside it. Only a time-out alone is the
// file's problem. So is one whose in-process read ran out of time with no
// ffprobe to fall back to (ProbeTimedOut): it would otherwise have no duration.
// And one whose header-less VBR MP3 went uncounted (LengthUncounted): its
// length would otherwise be ffprobe's estimate until a later scan.
func (r *readSlots) extract(ctx context.Context, root, absPath, ffprobePath string) *metadata.Metadata {
	md, shared := r.read(ctx, root, absPath, ffprobePath)
	if ctx.Err() != nil {
		// Stopped (an MP3's count cut short is uncounted): dropped, never
		// retried behind a read stuck on a share.
		return nil
	}
	if !shared || !(lengthTimedOut(md) || md.LengthUncounted()) {
		return md
	}
	// One retry at a time, each waiting for the reads under way to finish.
	r.alone.Lock()
	defer r.alone.Unlock()
	if ctx.Err() != nil {
		return nil // stopped while it waited: dropped by the scan
	}
	return r.readFile(ctx, root, absPath, ffprobePath, r.timeout)
}

// uncountedForNow reports whether md is a header-less VBR MP3 whose frames a
// later read may count (metadata.Metadata.LengthUncounted: a read failed, or
// it ran out of time beside other reads), so its book's length revision is
// left unstamped for the next scan to read it again. One whose count ran out
// of its own time read alone (CountOutOfTime: too large to count in that time
// on its disk or share) is not: a scan's read would only run out of time
// again, so the book keeps ffprobe's estimate until its files change or an
// admin's Re-read (server#452). extract has read one beside others again
// alone by then.
func uncountedForNow(md *metadata.Metadata) bool {
	return md.LengthUncounted() && !md.CountOutOfTime()
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

// openFaulted reports whether md's file could not be opened as a share fails
// (metadata.ShareFault: EIO, ESTALE, a permission): an answer about the share,
// not the file, so its book is kept as stored as for a read cut short, the
// problem recorded (a permission put on a file a read once hung on included,
// problemCleared). A file gone, or no regular file, is an answer.
func openFaulted(md *metadata.Metadata) bool {
	return md != nil && metadata.ShareFault(md.OpenErr) && !errors.Is(md.OpenErr, metadata.ErrNotRegular)
}

// lengthKnown reports whether md, one of a book's files' readings, leaves its
// length known whatever became of its tags: opened, its length not unread
// (lengthUnread), and read when the tags were cut short (ffprobe answered:
// metadata.Metadata.LengthRead). A book all of whose files' lengths are known
// but some tags not keeps its names as stored and takes its lengths
// (catalog.Book.LengthsRead).
func lengthKnown(md *metadata.Metadata) bool {
	return md != nil && md.OpenErr == nil && !lengthUnread(md) && (!md.TagsUnread() || md.LengthRead())
}

// read is metadata.ExtractTimeout holding a slot, and whether another read ran
// beside it; nil when ctx ends first.
func (r *readSlots) read(ctx context.Context, root, absPath, ffprobePath string) (*metadata.Metadata, bool) {
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
	md := r.readFile(ctx, root, absPath, ffprobePath, r.timeout)
	after := r.active.Add(-1)
	return md, before > 1 || after > 0 || r.started.Load() != start
}

// readFiles reads a folder book's files, in their order: the first is first
// (already read, as the book's primary file), the rest are read with slots, as
// many at once as they allow. With room for one at a time they are read on the
// caller's goroutine: an on-demand index reads on leadIndex's, which recovers a
// reader's panic (errIndexAborted) rather than the server going down with it.
func (s *Scanner) readFiles(ctx context.Context, lib catalog.Library, files []catalog.BookFile, first *metadata.Metadata, slots *readSlots, existed bool) []*metadata.Metadata {
	mds := make([]*metadata.Metadata, len(files))
	if len(files) == 0 {
		return mds
	}
	mds[0] = first
	if len(files) == 1 {
		return mds
	}
	// A stored book can still take fresh lengths when only tags timed out,
	// or a changed file list when only a length timed out. Stop scheduling
	// parts only when this read must keep the whole book.
	const tagsMissing, lengthMissing = 1, 2
	var missing atomic.Uint32
	mustKeep := func(md *metadata.Metadata) bool {
		if !existed || md == nil {
			return false
		}
		var flags uint32
		if openFaulted(md) || md.TagsUnread() {
			flags |= tagsMissing
		}
		if !lengthKnown(md) || uncountedForNow(md) {
			flags |= lengthMissing
		}
		if flags == 0 {
			return false
		}
		seen := missing.Or(flags) | flags
		return seen == tagsMissing|lengthMissing
	}
	if mustKeep(first) {
		return mds
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	read := func(i int) {
		if ctx.Err() == nil { // stopped: the scan drops this book's reading
			mds[i] = slots.extract(ctx, lib.Root, absOf(lib, files[i].RelPath), s.ffprobePath)
			if mustKeep(mds[i]) {
				cancel()
			}
		}
	}
	workers := min(slots.size(), len(files)-1)
	if workers <= 1 {
		for i := 1; i < len(files) && ctx.Err() == nil; i++ {
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
	root    string          // checked by a request publishing this scan read
	ctx     context.Context // the scan's: a request publishes the read only before a Stop, which drops one cut short (publish)
	claimed atomic.Bool     // either the scan batch or a waiting reader owns the write
	stored  chan struct{}
	err     error // the write's outcome, read after stored closes
	reused  bool  // an on-demand read already indexed this discovery
	// The book's flight (Scanner.startFlight), held from before the read until
	// the book is written or dropped; nil when the scan stopped first.
	flight *indexFlight
	// left is set once the scan stopped and left this read under way, for
	// the flight's waiters (indexFlight.scan).
	left atomic.Bool
}

// finishWrite releases the flight and gives the scan the actual write outcome.
// Joined requests may retry a dropped scan read.
func (r *bookRead) finishWrite(s *Scanner, err error) {
	r.err = err
	if r.flight != nil {
		if err != nil {
			err = errReadDropped
		}
		s.endFlight(r.flight, err)
	}
	close(r.stored)
}

// publish writes the scan's finished read for a request waiting on it (its
// waitFlight claimed it), ahead of the scan's batch. A Stop that cuts the
// write short drops the read (errReadDropped), as stopped drops one never
// written: neither the scan's failure nor its write.
func (r *bookRead) publish(s *Scanner) {
	r.book.ReadRoot = r.root
	_, err := s.cat.UpsertBook(r.ctx, r.book)
	if err != nil && r.ctx.Err() != nil {
		err = errReadDropped
	}
	r.finishWrite(s, err)
}

// drop ends a read the scan will not write (stopped), unless another
// writer claimed it first: then it waits for that write's outcome.
func (r *bookRead) drop(s *Scanner) {
	if r.claimed.CompareAndSwap(false, true) {
		r.finishWrite(s, errReadDropped)
	} else {
		<-r.stored
	}
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
