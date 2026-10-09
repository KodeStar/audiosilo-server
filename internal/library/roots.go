package library

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/diskspace"
)

// Root availability, for the admin console's "root unavailable" state.
//
// Checking a root means touching the filesystem, and a hard-mounted network
// share whose server went away can block a stat for minutes. So a check never
// makes its caller wait longer than rootProbeTimeout: the probe runs in its own
// goroutine, at most one per root at a time, and its answer is cached for
// rootProbeTTL. A probe that hasn't answered in time counts as unavailable ("not
// responding") - at once for every later check while it stays stuck, so a dead
// mount costs one timeout, not one per request; if it ever finishes, its answer
// serves the next check.

const (
	rootProbeTimeout = 2 * time.Second
	rootProbeTTL     = 15 * time.Second
)

// rootState is what a probe found.
type rootState struct {
	readable bool // a directory the server can list
	empty    bool // it lists no entries at all (an unmounted mount point)
	// The filesystem holding the root (when the root is readable and the OS
	// answered): its size and the bytes free to the server.
	diskTotal, diskFree uint64
	hasDisk             bool
}

// A probe's state and at are written before done closes, and read only after
// it has (the channel close orders them), so they need no lock; p.mu guards
// the map and started.
type rootProbe struct {
	done    chan struct{} // closed when the probe finished
	started time.Time
	state   rootState
	at      time.Time // when it finished
}

type rootProber struct {
	mu     sync.Mutex
	probes map[string]*rootProbe
	// stat is the filesystem check; a test swaps it for one that blocks.
	stat func(root string) rootState
}

func newRootProber() *rootProber {
	return &rootProber{probes: map[string]*rootProbe{}, stat: statRoot}
}

// statRoot opens root and reads at most one entry: enough to know it is a
// readable directory and whether it is empty, without listing a huge folder.
func statRoot(root string) rootState {
	f, err := os.Open(root)
	if err != nil {
		return rootState{}
	}
	defer func() { _ = f.Close() }() // read-only
	if info, err := f.Stat(); err != nil || !info.IsDir() {
		return rootState{}
	}
	names, err := f.Readdirnames(1)
	var st rootState
	switch {
	case len(names) > 0:
		st = rootState{readable: true}
	case errors.Is(err, io.EOF):
		st = rootState{readable: true, empty: true}
	default: // no permission, a dead mount
		return rootState{}
	}
	st.diskTotal, st.diskFree, st.hasDisk = diskspace.Of(root)
	return st
}

// check returns the root's state and whether a probe answered in time.
func (p *rootProber) check(root string) (rootState, bool) {
	p.mu.Lock()
	pr := p.probes[root]
	stale := pr != nil && pr.finished() && time.Since(pr.at) > rootProbeTTL
	if pr == nil || stale {
		pr = &rootProbe{done: make(chan struct{}), started: time.Now()}
		p.probes[root] = pr
		go func() {
			pr.state, pr.at = p.stat(root), time.Now()
			close(pr.done)
		}()
	}
	wait := rootProbeTimeout - time.Since(pr.started)
	p.mu.Unlock()

	if pr.finished() {
		return pr.state, true
	}
	if wait <= 0 {
		return rootState{}, false // already stuck past the timeout: don't wait again
	}
	select {
	case <-pr.done:
		return pr.state, true
	case <-time.After(wait):
		return rootState{}, false
	}
}

// forget drops a finished probe's cached answer so the next check looks again.
// A probe still in flight is kept: starting a second one would only pile up
// goroutines blocked on the same dead mount.
func (p *rootProber) forget(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr := p.probes[root]; pr != nil && pr.finished() {
		delete(p.probes, root)
	}
}

func (pr *rootProbe) finished() bool {
	select {
	case <-pr.done:
		return true
	default:
		return false
	}
}

// Disk is the space on the filesystem holding a library's root.
type Disk struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"` // free to the server (not counting space reserved for root)
}

// RootDisk reports the space on the filesystem holding lib's root, from the
// same bounded, cached probe as RootAvailable; ok is false when the root
// doesn't answer or the OS doesn't say.
func (s *Scanner) RootDisk(lib catalog.Library) (Disk, bool) {
	st, answered := s.roots.check(lib.Root)
	if !answered || !st.hasDisk {
		return Disk{}, false
	}
	return Disk{Total: st.diskTotal, Free: st.diskFree}, true
}

// RootsAvailable runs RootAvailable for every library in parallel, so a dead
// network share costs the whole list at most one probe timeout. indexed maps a
// library id to its indexed book count.
func (s *Scanner) RootsAvailable(libs []catalog.Library, indexed map[int64]int) map[int64]bool {
	out := make(map[int64]bool, len(libs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range libs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := s.RootAvailable(l, indexed[l.ID])
			mu.Lock()
			out[l.ID] = ok
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// RootAvailable reports whether lib's root can be read right now. It is false
// when the last scan stopped at the unavailable-root guard (nothing was pruned;
// a rescan clears it once the root is back), when the root is missing, unreadable
// or not responding, or when it is an empty directory while `indexed` books are
// still indexed under it (what an unmounted network share looks like). It waits
// at most rootProbeTimeout.
func (s *Scanner) RootAvailable(lib catalog.Library, indexed int) bool {
	if s.Progress(lib.ID).Unavailable {
		return false
	}
	st, answered := s.roots.check(lib.Root)
	if !answered || !st.readable {
		return false
	}
	return !(st.empty && indexed > 0)
}
