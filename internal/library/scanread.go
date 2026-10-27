package library

import (
	"context"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// readSlots bounds how many audio files a scan reads at once (tags, then
// ffprobe): one slot per read under way. An on-demand index reads with one.
type readSlots chan struct{}

// newReadSlots returns n slots (at least one).
func newReadSlots(n int) readSlots {
	return make(readSlots, max(n, 1))
}

// extract is metadata.ExtractContext holding a slot while it reads; nil when
// ctx ends while it waits for one (stopped: the scan drops the reading).
func (r readSlots) extract(ctx context.Context, absPath, ffprobePath string) *metadata.Metadata {
	select {
	case r <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-r }()
	md, _ := metadata.ExtractContext(ctx, absPath, ffprobePath)
	return md
}

// readFiles reads a folder book's files, in their order: the first is first
// (already read, as the book's primary file), the rest are read with slots, as
// many at once as they allow. With room for one at a time they are read on the
// caller's goroutine: an on-demand index reads on its request's, where net/http
// recovers a reader's panic (leadIndex's errIndexAborted) rather than the server
// going down with it.
func (s *Scanner) readFiles(ctx context.Context, lib catalog.Library, files []catalog.BookFile, first *metadata.Metadata, slots readSlots) []*metadata.Metadata {
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
	workers := min(cap(slots), len(files)-1)
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
