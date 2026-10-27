package library

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// readSlots bounds how many audio files a scan reads at once (tags, then
// ffprobe): one slot per read under way. An on-demand index reads with one.
type readSlots struct {
	ch chan struct{}
	// alone is held by a read retried on its own (extract), while it takes every slot.
	alone sync.Mutex
	// timeout is each ffprobe run's (metadata.ProbeTimeout; tests shorten it).
	timeout time.Duration
}

// newReadSlots returns n slots (at least one).
func newReadSlots(n int) *readSlots {
	return &readSlots{ch: make(chan struct{}, max(n, 1)), timeout: metadata.ProbeTimeout}
}

// size is how many files may be read at once.
func (r *readSlots) size() int { return cap(r.ch) }

// extract is metadata.ExtractTimeout holding a slot while it reads; nil when
// ctx ends while it waits for one (stopped: the scan drops the reading).
//
// A read whose ffprobe ran out of time while others were reading is read again
// on its own, holding every slot: files sharing a slow disk or a busy share can
// each take longer than ffprobe is given, and a healthy file must not be
// recorded as a read problem because others were read beside it. Only a second
// time-out, alone, is the file's problem.
func (r *readSlots) extract(ctx context.Context, absPath, ffprobePath string) *metadata.Metadata {
	if !r.take(ctx, 1) {
		return nil
	}
	md, _ := metadata.ExtractTimeout(ctx, absPath, ffprobePath, r.timeout)
	r.give(1)
	if r.size() == 1 || !errors.Is(md.ProbeErr, metadata.ErrProbeTimeout) {
		return md
	}
	// One retry at a time, each waiting for every read under way to finish.
	r.alone.Lock()
	defer r.alone.Unlock()
	if !r.take(ctx, r.size()) {
		return md // stopped: dropped by the scan
	}
	defer r.give(r.size())
	again, _ := metadata.ExtractTimeout(ctx, absPath, ffprobePath, r.timeout)
	return again
}

// take takes n slots, one by one; false (none held) when ctx ends first.
func (r *readSlots) take(ctx context.Context, n int) bool {
	for i := range n {
		select {
		case r.ch <- struct{}{}:
		case <-ctx.Done():
			r.give(i)
			return false
		}
	}
	return true
}

// give hands back n slots.
func (r *readSlots) give(n int) {
	for range n {
		<-r.ch
	}
}

// readFiles reads a folder book's files, in their order: the first is first
// (already read, as the book's primary file), the rest are read with slots, as
// many at once as they allow. With room for one at a time they are read on the
// caller's goroutine: an on-demand index reads on its request's, where net/http
// recovers a reader's panic (leadIndex's errIndexAborted) rather than the server
// going down with it.
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
