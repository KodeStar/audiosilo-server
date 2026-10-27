package library

import (
	"context"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// readSlots bounds how many audio files a scan reads at once (tags, then
// ffprobe): one slot per read under way. A nil readSlots reads one file at a
// time, on the caller's goroutine (an on-demand index).
type readSlots chan struct{}

// newReadSlots returns n slots (at least one).
func newReadSlots(n int) readSlots {
	return make(readSlots, max(n, 1))
}

// extract is metadata.ExtractContext holding a slot while it reads.
func (r readSlots) extract(ctx context.Context, absPath, ffprobePath string) *metadata.Metadata {
	if r != nil {
		r <- struct{}{}
		defer func() { <-r }()
	}
	md, _ := metadata.ExtractContext(ctx, absPath, ffprobePath)
	return md
}

// readFiles reads a folder book's files, in their order: the first is first
// (already read, as the book's primary file), the rest are read with slots, as
// many at once as they allow (in turn without slots).
func (s *Scanner) readFiles(ctx context.Context, lib catalog.Library, files []catalog.BookFile, first *metadata.Metadata, slots readSlots) []*metadata.Metadata {
	mds := make([]*metadata.Metadata, len(files))
	if len(files) == 0 {
		return mds
	}
	mds[0] = first
	if slots == nil {
		for i := 1; i < len(files); i++ {
			mds[i] = slots.extract(ctx, absOf(lib, files[i].RelPath), s.ffprobePath)
		}
		return mds
	}
	// A goroutine per slot at most, each taking the next file not yet read.
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(cap(slots), len(files)-1) {
		wg.Go(func() {
			for i := range next {
				if ctx.Err() != nil {
					continue // stopped: the scan drops this book's reading
				}
				mds[i] = slots.extract(ctx, absOf(lib, files[i].RelPath), s.ffprobePath)
			}
		})
	}
	for i := 1; i < len(files); i++ {
		next <- i
	}
	close(next)
	wg.Wait()
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
