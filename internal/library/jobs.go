package library

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The job queue: every library scan the server starts (on startup, on a schedule,
// when an admin asks, when a library or folder setting changes) waits here and
// runs one at a time, so scans never compete for the disk or the database's one
// writer, and each is recorded in scan_runs. Jobs live in memory; a queued job
// doesn't survive a restart (the startup scans and schedules cover that).
//
// Coalescing: asking for a library that already waits returns the waiting job. Asking
// for one that is running queues it to run again once (a setting changed mid-scan,
// so the running scan may have read the old one), unless the ask is a schedule or a
// startup scan, which has nothing new for a second pass to see.

// Triggers: why a scan was queued.
const (
	TriggerManual   = "manual"   // an admin asked
	TriggerSchedule = "schedule" // the library's scan schedule
	TriggerStartup  = "startup"  // the server started
	TriggerChange   = "change"   // a library or folder setting changed
)

// scanTimeout caps a requested scan (see jobContext).
const scanTimeout = time.Hour

// scheduleTick is how often the scheduler looks for due scans.
const scheduleTick = time.Minute

// Job is a queued or running scan.
type Job struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"` // "scan"
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	Trigger     string `json:"trigger"`
	StartedBy   *int64 `json:"started_by"` // the admin who asked; nil for a schedule or startup
	QueuedAt    string `json:"queued_at"`
	StartedAt   string `json:"started_at,omitempty"`
	// RunID is the job's scan_runs row once it runs.
	RunID int64 `json:"run_id,omitempty"`
	// Progress is a running job's progress.
	Progress *ScanProgress `json:"progress,omitempty"`

	cancel context.CancelFunc
}

// jobQueue is the queue's state; Scanner.mu guards it.
type jobQueue struct {
	queued  []*Job
	running *Job
	nextID  int64
	wake    chan struct{} // signalled (non-blocking) when a job is queued
	anchor  time.Time     // when the scheduler started: what a never-scanned schedule counts from
	// skipped is when a library's due scheduled scan was last dropped without a run
	// of its own: cancelled while it waited, or folded into a scan of the library
	// already running. The schedule counts from it as from a scan start, so the next
	// tick doesn't queue the same slot again.
	skipped map[int64]time.Time
}

func newJobQueue() jobQueue {
	return jobQueue{wake: make(chan struct{}, 1), skipped: map[int64]time.Time{}}
}

// queuedFor reports whether a job for the library waits.
func (q *jobQueue) queuedFor(libID int64) bool {
	for _, j := range q.queued {
		if j.LibraryID == libID {
			return true
		}
	}
	return false
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// EnqueueAll queues a scan of every library, in display order, and returns the
// jobs (Health's "Check again", the startup scans).
func (s *Scanner) EnqueueAll(ctx context.Context, trigger string, startedBy *int64) ([]Job, error) {
	libs, err := s.cat.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, len(libs))
	for i, l := range libs {
		jobs[i] = s.Enqueue(l, trigger, startedBy)
	}
	return jobs, nil
}

// Enqueue queues a scan of lib (see the coalescing rules above) and returns the
// job that will scan it. The library reads as queued (or running) before this
// returns, so a status poll right after the request that queued it sees the scan.
// startedBy is the admin who asked, nil otherwise.
func (s *Scanner) Enqueue(lib catalog.Library, trigger string, startedBy *int64) Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := &s.jobs
	for _, j := range q.queued {
		if j.LibraryID == lib.ID {
			return j.snapshot()
		}
	}
	if r := q.running; r != nil && r.LibraryID == lib.ID && (trigger == TriggerSchedule || trigger == TriggerStartup) {
		if trigger == TriggerSchedule {
			// The running scan stands in for this slot, though it may have started
			// before the slot was due.
			q.skipped[lib.ID] = time.Now()
		}
		return s.runningSnapshot()
	}
	q.nextID++
	j := &Job{ID: q.nextID, Kind: "scan", LibraryID: lib.ID, LibraryName: lib.Name,
		Trigger: trigger, StartedBy: startedBy, QueuedAt: now()}
	q.queued = append(q.queued, j)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return j.snapshot()
}

// Jobs returns the running job (nil when idle) and the queued ones, in order.
func (s *Scanner) Jobs() (*Job, []Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := make([]Job, len(s.jobs.queued))
	for i, j := range s.jobs.queued {
		queued[i] = j.snapshot()
	}
	if s.jobs.running == nil {
		return nil, queued
	}
	r := s.runningSnapshot()
	return &r, queued
}

// Cancel drops a queued job or stops the running one (the scan stops before it
// prunes anything, and is recorded as cancelled; a prune already under way
// finishes first). A dropped scheduled scan skips its slot. False when no such
// job exists.
func (s *Scanner) Cancel(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := &s.jobs
	for i, j := range q.queued {
		if j.ID != id {
			continue
		}
		q.queued = append(q.queued[:i], q.queued[i+1:]...)
		if j.Trigger == TriggerSchedule {
			q.skipped[j.LibraryID] = time.Now()
		}
		return true
	}
	if r := q.running; r != nil && r.ID == id && r.cancel != nil {
		r.cancel()
		return true
	}
	return false
}

// CancelLibrary drops a library's queued scans and stops its running one: what
// deleting the library leaves them, since there is nothing left to index into.
func (s *Scanner) CancelLibrary(libID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := &s.jobs
	q.queued = slices.DeleteFunc(q.queued, func(j *Job) bool { return j.LibraryID == libID })
	if r := q.running; r != nil && r.LibraryID == libID && r.cancel != nil {
		r.cancel()
	}
	delete(q.skipped, libID)
}

// snapshot copies a job for a caller (without its cancel func).
func (j *Job) snapshot() Job {
	c := *j
	c.cancel = nil
	return c
}

// runningSnapshot copies the running job with its library's progress. mu held.
func (s *Scanner) runningSnapshot() Job {
	j := s.jobs.running.snapshot()
	p := s.progressLocked(j.LibraryID)
	j.Progress = &p
	return j
}

// Start runs the job queue and the scheduler until ctx ends. Runs a previous
// server left open are marked interrupted first.
func (s *Scanner) Start(ctx context.Context) {
	if err := s.cat.InterruptScanRuns(ctx); err != nil {
		s.log.Warn("mark interrupted scans failed", "err", err)
	}
	s.mu.Lock()
	s.jobs.anchor = time.Now()
	s.mu.Unlock()
	go s.work(ctx)
	go s.schedule(ctx)
}

// work runs queued jobs one at a time until ctx ends.
func (s *Scanner) work(ctx context.Context) {
	for {
		s.mu.Lock()
		if len(s.jobs.queued) == 0 {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.jobs.wake:
				continue
			}
		}
		j := s.jobs.queued[0]
		s.jobs.queued = s.jobs.queued[1:]
		jctx, cancel := jobContext(ctx, j.Trigger)
		j.cancel, j.StartedAt = cancel, now()
		s.jobs.running = j
		// It reads as running from here (Scan resets the counters when it starts).
		p := s.progress[j.LibraryID]
		p.Running = true
		s.progress[j.LibraryID] = p
		s.mu.Unlock()

		s.run(ctx, jctx, j)
		cancel()

		s.mu.Lock()
		s.jobs.running = nil
		p = s.progress[j.LibraryID]
		p.Running = false
		s.progress[j.LibraryID] = p
		s.mu.Unlock()
	}
}

// jobContext bounds a job's scan: an hour (scanTimeout) for a scan someone or
// something asked for, none for the startup scan, which is a library's full index
// after a restart (its first, for one declared in config) and runs to the end, as
// it always has. Either way it ends with ctx, and Cancel stops it.
func jobContext(ctx context.Context, trigger string) (context.Context, context.CancelFunc) {
	if trigger == TriggerStartup {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, scanTimeout)
}

// run scans a job's library (as it is now: renamed, re-rooted or deleted since it
// was queued) and records the run.
func (s *Scanner) run(ctx, jctx context.Context, j *Job) {
	lib, err := s.cat.GetLibrary(ctx, j.LibraryID)
	if err != nil {
		s.updateProgress(j.LibraryID, func(p *ScanProgress) { p.Running = false })
		if !errors.Is(err, catalog.ErrNotFound) {
			s.log.Warn("scan job: load library failed", "library", j.LibraryID, "err", err)
		}
		return
	}
	// The run is recorded even if the server stops mid-scan, so writes use a
	// context that outlives ctx.
	db := context.WithoutCancel(ctx)
	runID, err := s.cat.StartScanRun(db, lib.ID, j.Trigger, j.StartedBy)
	if err != nil {
		s.log.Warn("record scan start failed", "library", lib.Name, "err", err)
	}
	s.mu.Lock()
	j.RunID = runID
	s.mu.Unlock()

	res, err := s.Scan(jctx, *lib)
	status := runStatus(res, err, ctx, jctx)
	if err != nil && status != catalog.RunCancelled && status != catalog.RunInterrupted {
		s.log.Warn("scan failed", "library", lib.Name, "err", err)
	}
	log := append(res.Log, closingEvent(status, err, res))
	if runID != 0 {
		if err := s.cat.FinishScanRun(db, runID, status, res.ScanCounts, log); err != nil {
			s.log.Warn("record scan result failed", "library", lib.Name, "err", err)
		}
	}
}

// runStatus is how a scan ended, for its scan_runs row.
func runStatus(res *ScanResult, err error, serverCtx, jobCtx context.Context) string {
	switch {
	case err == nil && res.Partial:
		return catalog.RunPartial
	case err == nil:
		return catalog.RunOK
	case errors.Is(err, ErrLibraryUnavailable):
		return catalog.RunUnavailable
	case serverCtx.Err() != nil:
		return catalog.RunInterrupted
	case errors.Is(jobCtx.Err(), context.Canceled):
		return catalog.RunCancelled
	default: // including the scan timeout
		return catalog.RunFailed
	}
}

// closingEvent is the last line of a scan's log: how it ended, from the same
// status its scan_runs row records.
func closingEvent(status string, err error, res *ScanResult) catalog.RunEvent {
	switch status {
	case catalog.RunOK, catalog.RunPartial:
		return newEvent("info", "finished", func(e *catalog.RunEvent) { e.Count = res.Books })
	case catalog.RunUnavailable:
		// The cause alone: the console words "stopped, nothing was removed" itself.
		return newEvent("error", "unavailable", func(e *catalog.RunEvent) {
			e.Detail = strings.TrimPrefix(err.Error(), ErrLibraryUnavailable.Error()+": ")
		})
	case catalog.RunCancelled, catalog.RunInterrupted:
		return newEvent("warn", status, nil)
	default:
		return newEvent("error", "failed", func(e *catalog.RunEvent) { e.Detail = err.Error() })
	}
}

// NextScans returns when each of libs with a schedule is next due, by library id
// (libraries without a schedule are absent).
func (s *Scanner) NextScans(ctx context.Context, libs []catalog.Library) (map[int64]time.Time, error) {
	last, err := s.cat.LastScanStarts(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	anchor := s.jobs.anchor
	skipped := maps.Clone(s.jobs.skipped)
	s.mu.Unlock()
	if anchor.IsZero() {
		anchor = time.Now()
	}
	out := map[int64]time.Time{}
	for _, l := range libs {
		sch, err := ParseSchedule(l.ScanSchedule)
		if err != nil || sch.Off() {
			continue
		}
		from := last[l.ID]
		if from.IsZero() {
			from = anchor
		}
		if sk := skipped[l.ID]; sk.After(from) {
			from = sk // a dropped slot counts as scanned (see jobQueue.skipped)
		}
		out[l.ID] = sch.Next(from, time.Now())
	}
	return out, nil
}

// schedule queues each library's scan when its schedule says it's due, checking
// every scheduleTick until ctx ends.
func (s *Scanner) schedule(ctx context.Context) {
	t := time.NewTicker(scheduleTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.queueDue(ctx, time.Now())
		}
	}
}

// queueDue queues the scans due at now.
func (s *Scanner) queueDue(ctx context.Context, at time.Time) {
	libs, err := s.cat.ListLibraries(ctx)
	if err != nil {
		s.log.Warn("scan schedule check failed", "err", err)
		return
	}
	next, err := s.NextScans(ctx, libs)
	if err != nil {
		s.log.Warn("scan schedule check failed", "err", err)
		return
	}
	for _, l := range libs {
		if due, ok := next[l.ID]; ok && !due.After(at) {
			s.Enqueue(l, TriggerSchedule, nil)
		}
	}
}
