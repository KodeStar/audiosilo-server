package api

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// dirNames keeps, for a few seconds, the names of the folders the stream and
// /fs have listed to check a path's spelling (onDiskAsGiven), so a path's
// check costs the same whether or not the disk has it in another case.
// Every path the index doesn't hold walks the same listings (a missing one and a
// case variant both stop at the first name not listed), and a folder is read at
// most once in dirNamesTTL however many requests ask (concurrent ones wait for
// the read in flight): a client probing names under a large folder can neither
// time the read against a missing path's nor make the server read the folder
// again on every request. Bounded: dirNamesMax folders and dirNamesBytes of
// names in all, the oldest dropped first; a folder whose names alone pass the
// bound is read for its request only. A failed or given-up read is never kept:
// whoever waited on it reads for itself. Nothing invalidates an entry (no
// filesystem watcher here): it lives dirNamesTTL from its read, so a name added
// on disk answers within that, and a path the index holds is never looked up.
type dirNames struct {
	read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)
	now  func() time.Time
	ttl  time.Duration
	max  int // folders kept
	cap  int // bytes of names kept (dirListing.cost)

	mu    sync.Mutex
	size  int        // bytes held by the kept, read entries
	order *list.List // of *dirListing, oldest first: each lives ttl from its read's start
	byDir map[string]*list.Element
}

const (
	dirNamesTTL   = 3 * time.Second
	dirNamesMax   = 256
	dirNamesBytes = 16 << 20
	// dirNameCost is what a name holds beside its bytes (its string header in
	// the slice).
	dirNameCost = 16
)

// dirListing is one folder's read: names and err are set before ready closes.
type dirListing struct {
	dir   string
	at    time.Time
	ready chan struct{}
	names []string
	err   error
	cost  int // its names' bytes (dirNameCost each beside them), set once kept
}

func newDirNames(read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)) *dirNames {
	return &dirNames{read: read, now: time.Now, ttl: dirNamesTTL, max: dirNamesMax, cap: dirNamesBytes,
		order: list.New(), byDir: map[string]*list.Element{}}
}

// names is dir's names, unsorted, as read at most ttl ago, waiting at most
// timeout for a read: the kept one, the one in flight, or this request's own.
// A wait that runs out is metadata.ErrNotAnswered, as a read's would be.
func (c *dirNames) names(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
	start := c.now()
	c.mu.Lock()
	c.expire(start)
	if el, ok := c.byDir[dir]; ok {
		l := el.Value.(*dirListing)
		c.mu.Unlock()
		return c.await(ctx, l, timeout)
	}
	l := &dirListing{dir: dir, at: start, ready: make(chan struct{})}
	c.byDir[dir] = c.order.PushBack(l)
	c.trim()
	c.mu.Unlock()

	l.names, l.err = c.read(ctx, dir, timeout)
	cost := 0
	for _, n := range l.names {
		cost += len(n) + dirNameCost
	}
	close(l.ready)
	c.mu.Lock()
	if el, ok := c.byDir[dir]; ok && el.Value == l {
		if l.err != nil || cost > c.cap {
			c.drop(el)
		} else {
			l.cost = cost
			c.size += cost
			c.trim()
		}
	}
	c.mu.Unlock()
	return l.names, l.err
}

// await is l's names once its read is done, or this request's own read when
// that one failed (its client may have left; its error says nothing of this
// request's), within timeout in all.
func (c *dirNames) await(ctx context.Context, l *dirListing, timeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(timeout)
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-l.ready:
		if l.err == nil {
			return l.names, nil
		}
		return c.read(ctx, l.dir, max(time.Until(deadline), time.Nanosecond))
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("%s: %w", l.dir, metadata.ErrNotAnswered)
	}
}

// expire drops the entries read ttl or more before now. Caller holds mu.
func (c *dirNames) expire(now time.Time) {
	for el := c.order.Front(); el != nil && now.Sub(el.Value.(*dirListing).at) >= c.ttl; el = c.order.Front() {
		c.drop(el)
	}
}

// trim drops the oldest entries until the cache is within its bounds. Caller
// holds mu.
func (c *dirNames) trim() {
	for c.order.Len() > 0 && (c.order.Len() > c.max || c.size > c.cap) {
		c.drop(c.order.Front())
	}
}

// drop forgets el's entry (a request waiting on it still gets its read).
// Caller holds mu.
func (c *dirNames) drop(el *list.Element) {
	l := c.order.Remove(el).(*dirListing)
	delete(c.byDir, l.dir)
	c.size -= l.cost // 0 while in flight (its read finds itself gone)
}
