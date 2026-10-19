package modules

import (
	"context"
	"sync"
)

// Runner runs each module's background work (Module.Start) while the module is
// on. Its zero value is ready: nothing runs before Start, and Sync brings the
// runs in line with the toggles after each change. Safe for concurrent use.
type Runner struct {
	mu   sync.Mutex
	ctx  context.Context // Start's context; nil until then
	mods []Module
	deps Deps
	runs map[string]*run
}

// run is one run of a module's Start.
type run struct {
	cancel  context.CancelFunc
	done    chan struct{} // closed when Start has returned
	stopped bool          // cancelled by Sync (the module turned off)
}

// Start starts the background work of each module in ms that is on, for ctx's
// lifetime, with d (each run's Log tagged with its module's id); from then on
// Sync starts and stops it. Call once.
func (r *Runner) Start(ctx context.Context, ms []Module, d Deps, on func(id string) bool) {
	r.mu.Lock()
	r.ctx, r.mods, r.deps = ctx, ms, d
	r.runs = map[string]*run{}
	r.mu.Unlock()
	r.Sync(on)
}

// Sync brings each module's background work in line with on: it starts a
// module turned on and cancels one turned off. It does not wait for a stopping
// module (a settings save calls it and answers at once); a module turned back
// on meanwhile starts once the old run has returned, so two runs of one module
// never overlap. Nothing before Start.
func (r *Runner) Sync(on func(id string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return
	}
	for _, m := range r.mods {
		if m.Start == nil {
			continue
		}
		cur := r.runs[m.ID]
		switch isOn := on(m.ID); {
		case isOn && (cur == nil || cur.stopped):
			r.runs[m.ID] = r.launch(m, cur)
		case !isOn && cur != nil && !cur.stopped:
			cur.stopped = true
			cur.cancel()
		}
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
		if prev != nil {
			select {
			case <-prev.done:
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		// A module's failure is its own: it stops that module, never the server.
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
