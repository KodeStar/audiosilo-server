package modules

import (
	"context"
	"sync"
)

// Runner runs each module's background work (Module.Start) while the module is
// on. Its zero value is ready: nothing runs before Start, Sync brings the runs
// in line with the toggles after each change, and Stop ends them all. Safe for
// concurrent use.
type Runner struct {
	mu   sync.Mutex
	ctx  context.Context    // Start's context, ended by Stop; nil until Start
	stop context.CancelFunc // ends ctx
	mods []Module
	deps Deps
	runs map[string]*run
}

// run is one run of a module's Start.
type run struct {
	cancel  context.CancelFunc
	done    chan struct{} // closed once Start, and every earlier run of the module, has returned
	stopped bool          // cancelled by Sync (the module turned off)
}

// ended reports whether the run has returned (Start returned, or panicked).
func (cur *run) ended() bool {
	select {
	case <-cur.done:
		return true
	default:
		return false
	}
}

// Start starts the background work of each module in ms that is on, for ctx's
// lifetime (or until Stop), with d (each run's Log tagged with its module's
// id); from then on Sync starts and stops it. Call once: a second call is
// ignored.
func (r *Runner) Start(ctx context.Context, ms []Module, d Deps, on func(id string) bool) {
	r.mu.Lock()
	if r.ctx != nil {
		r.mu.Unlock()
		return
	}
	r.ctx, r.stop = context.WithCancel(ctx)
	r.mods, r.deps = ms, d
	r.runs = map[string]*run{}
	r.mu.Unlock()
	r.Sync(on)
}

// Sync brings each module's background work in line with on: it starts a
// module turned on (or whose run ended by itself while on) and cancels one
// turned off. It does not wait for a stopping module (a settings save calls it
// and answers at once); a module turned back on meanwhile starts once the old
// run has returned, so two runs of one module never overlap. Nothing before
// Start or after Stop.
func (r *Runner) Sync(on func(id string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil || r.ctx.Err() != nil {
		return
	}
	for _, m := range r.mods {
		if m.Start == nil {
			continue
		}
		cur := r.runs[m.ID]
		switch isOn := on(m.ID); {
		case isOn && (cur == nil || cur.stopped || cur.ended()):
			r.runs[m.ID] = r.launch(m, cur)
		case !isOn && cur != nil && !cur.stopped:
			cur.stopped = true
			cur.cancel()
		}
	}
}

// Stop cancels every module's background work and waits for each run to
// return (the launcher calls it at shutdown, before the database closes). Sync
// starts nothing after it.
func (r *Runner) Stop() {
	r.mu.Lock()
	if r.stop == nil {
		r.mu.Unlock()
		return
	}
	r.stop()
	done := make([]chan struct{}, 0, len(r.runs))
	for _, cur := range r.runs {
		done = append(done, cur.done)
	}
	r.mu.Unlock()
	for _, d := range done {
		<-d
	}
}

// launch starts a run of m.Start under the Runner's context, after prev (the
// module's last run, or nil) has returned. r.mu is held.
func (r *Runner) launch(m Module, prev *run) *run {
	ctx, cancel := context.WithCancel(r.ctx)
	cur := &run{cancel: cancel, done: make(chan struct{})}
	d := r.deps
	if d.Log != nil {
		d.Log = d.Log.With("module", m.ID)
	}
	go func() {
		defer close(cur.done)
		defer cancel()
		// Wait for the last run even when this one is cancelled meanwhile: done
		// closes only after every earlier run's, so a later run waiting on this
		// one never overlaps one still returning.
		if prev != nil {
			<-prev.done
		}
		if ctx.Err() != nil {
			return
		}
		// A module's failure is its own: it ends this run, never the server.
		defer func() {
			if p := recover(); p != nil && d.Log != nil {
				d.Log.Error("module stopped by a panic", "panic", p)
			}
		}()
		if d.Log != nil {
			d.Log.Info("module started")
		}
		m.Start(ctx, d)
		if d.Log != nil {
			d.Log.Info("module stopped")
		}
	}()
	return cur
}
