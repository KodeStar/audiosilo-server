package metadata

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// DirNames keeps, for a few seconds, the names of the folders a check of a
// path's spelling has listed (library.SafeJoinListed for the stream, /fs and
// a book read on demand, catalog's ownEntry for Spelling's look at the disk),
// so the check costs the
// same whether or not the disk has the path in another spelling: a missing path
// and a variant (another case, another Unicode form) both walk the same
// listings and stop at the first name not listed. A folder is read once however
// many requests ask while its read is in flight (they wait for it, however long
// it takes: its own timeout ends it) and once in dirNamesTTL from that read's
// start: a client probing names under a large folder can neither time the read
// against a missing path's nor make the server read the folder again on every
// request. Bounded: dirNamesMax folders and dirNamesBytes of names kept, the
// oldest dropped first; a folder whose names alone pass the bound is read for
// the requests waiting on that read only. A failed or given-up read is never
// kept: whoever waited on it joins the next read of the folder (one of them
// makes it). Nothing invalidates an entry (no filesystem watcher here): it
// lives dirNamesTTL from its read's start, so a name added on disk answers
// within that.
type DirNames struct {
	read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)
	now  func() time.Time
	ttl  time.Duration
	max  int // folders kept
	cap  int // bytes of names kept (dirListing.cost)

	mu    sync.Mutex
	kept  int        // entries read and kept (the others are in flight)
	size  int        // bytes held by the kept entries
	order *list.List // of *dirListing, oldest read first
	byDir map[string]*list.Element
}

const (
	dirNamesTTL   = 3 * time.Second
	dirNamesMax   = 256
	dirNamesBytes = 16 << 20
	// dirNameCost is what a name holds beside its bytes (its string header in
	// the slice, counted over the slice's capacity).
	dirNameCost = 16
)

// dirListing is one folder's read: names and err are set before ready closes.
type dirListing struct {
	dir   string
	at    time.Time
	ready chan struct{}
	names []string
	err   error
	kept  bool // counted in DirNames.kept and size (cost), under mu
	cost  int  // its names' bytes and dirNameCost a slot of the slice
}

// done reports whether l's read has returned.
func (l *dirListing) done() bool {
	select {
	case <-l.ready:
		return true
	default:
		return false
	}
}

// NewDirNames is a DirNames whose folders are read by read
// (ReadDirNamesBounded, or a test's stand-in).
func NewDirNames(read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)) *DirNames {
	return &DirNames{read: read, now: time.Now, ttl: dirNamesTTL, max: dirNamesMax, cap: dirNamesBytes,
		order: list.New(), byDir: map[string]*list.Element{}}
}

// SetClockForTests sets the clock c ages its entries by, so a test that runs
// slowly never outlives the TTL it counts on. Tests only.
func (c *DirNames) SetClockForTests(now func() time.Time) {
	if !testing.Testing() {
		panic("metadata.SetClockForTests called outside a test binary")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Names is dir's names, unsorted, as read at most ttl ago, waiting at most
// timeout for a read: the kept one, the one in flight, or this request's own.
// A wait that runs out is ErrNotAnswered, as a read's would be.
func (c *DirNames) Names(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, lead := c.join(dir)
		if lead {
			return c.fill(ctx, l, max(time.Until(deadline), time.Nanosecond))
		}
		names, again, err := c.await(ctx, l, deadline)
		if !again {
			return names, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s: %w", dir, ErrNotAnswered)
		}
	}
}

// join is dir's entry and whether this request is to read it (lead): the one
// kept or in flight, else a new one. A failed read its reader has yet to drop
// is dropped here.
func (c *DirNames) join(dir string) (*dirListing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now() // under mu: order stays the order of the reads' starts
	c.expire(now)
	if el, ok := c.byDir[dir]; ok {
		l := el.Value.(*dirListing)
		if !l.done() || l.err == nil {
			return l, false
		}
		c.drop(el)
	}
	l := &dirListing{dir: dir, at: now, ready: make(chan struct{})}
	c.byDir[dir] = c.order.PushBack(l)
	return l, true
}

// fill makes l's read, then keeps it unless it failed or its names pass the
// bound (or it was dropped meanwhile: expired). A panic in the read is its
// error, so l never stays in flight for good (expire and trim pass over a read
// in flight).
func (c *DirNames) fill(ctx context.Context, l *dirListing, timeout time.Duration) ([]string, error) {
	func() {
		defer func() {
			if p := recover(); p != nil {
				l.names, l.err = nil, fmt.Errorf("%s: panic: %v", l.dir, p)
			}
		}()
		l.names, l.err = c.read(ctx, l.dir, timeout)
	}()
	cost := cap(l.names) * dirNameCost
	for _, n := range l.names {
		cost += len(n)
	}
	close(l.ready)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byDir[l.dir]; ok && el.Value == l {
		if l.err != nil || cost > c.cap {
			c.drop(el)
		} else {
			l.kept, l.cost = true, cost
			c.kept++
			c.size += cost
			c.trim()
		}
	}
	return l.names, l.err
}

// await is l's names once its read is done, within deadline; again when that
// read failed (its client may have left; its error says nothing of this
// request's): the caller then joins the folder's next read, or makes it.
func (c *DirNames) await(ctx context.Context, l *dirListing, deadline time.Time) (names []string, again bool, err error) {
	if !l.done() { // a kept read answers at once, whatever the time left
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		select {
		case <-l.ready:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-t.C:
			return nil, false, fmt.Errorf("%s: %w", l.dir, ErrNotAnswered)
		}
	}
	if l.err != nil {
		return nil, true, nil
	}
	return l.names, false, nil
}

// expire drops the entries read ttl or more before now, a read still in flight
// aside (waited on however long it takes: its own timeout ends it). Caller
// holds mu.
func (c *DirNames) expire(now time.Time) {
	for el := c.order.Front(); el != nil && now.Sub(el.Value.(*dirListing).at) >= c.ttl; {
		next := el.Next()
		if el.Value.(*dirListing).done() {
			c.drop(el)
		}
		el = next
	}
}

// trim drops the oldest kept entries until those kept are within the bounds (a
// read in flight holds nothing yet). Caller holds mu.
func (c *DirNames) trim() {
	for el := c.order.Front(); el != nil && (c.kept > c.max || c.size > c.cap); {
		next := el.Next()
		if el.Value.(*dirListing).kept {
			c.drop(el)
		}
		el = next
	}
}

// drop forgets el's entry (a request waiting on it still gets its read).
// Caller holds mu.
func (c *DirNames) drop(el *list.Element) {
	l := c.order.Remove(el).(*dirListing)
	delete(c.byDir, l.dir)
	if l.kept {
		l.kept = false
		c.kept--
		c.size -= l.cost
	}
}
