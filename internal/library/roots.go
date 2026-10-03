package library

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Root availability, for the admin console's "root unavailable" state.
//
// Checking a root means touching the filesystem, and a hard-mounted network
// share whose server went away can block a stat for minutes. So a check never
// makes its caller wait longer than rootProbeTimeout: the probe runs in its own
// goroutine, at most one per root at a time, and its answer is cached for
// rootProbeTTL. A probe that hasn't answered in time counts as unavailable ("not
// responding"); if it later finishes, its answer serves the next check.

const (
	rootProbeTimeout = 2 * time.Second
	rootProbeTTL     = 15 * time.Second
)

// rootState is what a probe found.
type rootState struct {
	readable bool // a directory the server can list
	empty    bool // it lists no entries at all (an unmounted mount point)
}

type rootProbe struct {
	done  chan struct{} // closed when the probe finished
	state rootState
	at    time.Time // when it finished
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
	switch {
	case len(names) > 0:
		return rootState{readable: true}
	case errors.Is(err, io.EOF):
		return rootState{readable: true, empty: true}
	default: // no permission, a dead mount
		return rootState{}
	}
}

// check returns the root's state and whether a probe answered in time.
func (p *rootProber) check(root string) (rootState, bool) {
	p.mu.Lock()
	pr := p.probes[root]
	stale := pr != nil && pr.finished() && time.Since(pr.at) > rootProbeTTL
	if pr == nil || stale {
		pr = &rootProbe{done: make(chan struct{})}
		p.probes[root] = pr
		go func() {
			st := p.stat(root)
			p.mu.Lock()
			pr.state, pr.at = st, time.Now()
			p.mu.Unlock()
			close(pr.done)
		}()
	}
	p.mu.Unlock()

	select {
	case <-pr.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return pr.state, true
	case <-time.After(rootProbeTimeout):
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
