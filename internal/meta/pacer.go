package meta

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Pacer keeps one bulk run's requests to the remote service evenly spaced, under a
// rate, and counts what the service asked it to wait: how many waits, and the wall
// time they cover (the run's workers wait side by side, so their waits overlap). It
// goes in the run's context (WithPacer) rather than on the Service, so only the run
// is paced: an admin's dialog and a listener's lookup beside it are not, and don't
// spend its slots.
//
// The spacing happens in the remote leg's transport (pacedTransport), so a request a
// local copy answers (mirror mode) takes no slot, one it fails over to the remote
// service does, and so does each retry and each redirect.
type Pacer struct {
	interval time.Duration
	log      *slog.Logger
	sleep    func(ctx context.Context, d time.Duration) error
	now      func() time.Time

	mu     sync.Mutex
	next   time.Time // the next free slot
	waits  int
	waited time.Duration // the wall time the waits cover, overlaps counted once
	until  time.Time     // when the latest wait asked for ends
}

// NewPacer returns a pacer of at most perSecond requests a second, logging each wait
// the service asks for at INFO on log.
func NewPacer(perSecond int, log *slog.Logger) *Pacer {
	if log == nil {
		log = slog.Default()
	}
	return &Pacer{interval: time.Second / time.Duration(perSecond), log: log, sleep: sleepCtx, now: time.Now}
}

type pacerKey struct{}

// WithPacer puts p in ctx, for the client's requests made under it.
func WithPacer(ctx context.Context, p *Pacer) context.Context {
	return context.WithValue(ctx, pacerKey{}, p)
}

func pacerOf(ctx context.Context) *Pacer {
	p, _ := ctx.Value(pacerKey{}).(*Pacer)
	return p
}

// wait takes the next slot and waits for it, or for ctx (its error then). Slots are
// handed out in turn and an idle pacer's aren't saved up, so a run never bursts.
func (p *Pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := p.now()
	slot := p.next
	if slot.Before(now) {
		slot = now
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()
	if d := slot.Sub(now); d > 0 {
		return p.sleep(ctx, d)
	}
	return nil
}

// noteWait records and logs a wait the answer err asked for, with the run's total.
// The total is wall time: the part of this wait another worker's already covers
// is not added again (six workers told to wait 20 s together waited 20 s, not 2
// minutes; server#307).
func (p *Pacer) noteWait(err error, wait time.Duration) {
	p.mu.Lock()
	p.waits++
	now := p.now()
	if end := now.Add(wait); end.After(p.until) {
		from := now
		if p.until.After(from) {
			from = p.until
		}
		p.waited += end.Sub(from)
		p.until = end
	}
	waits, waited := p.waits, p.waited
	p.mu.Unlock()
	status := 0
	var st *statusError
	if errors.As(err, &st) {
		status = st.code
	}
	p.log.Info("community metadata: waiting as the service asked", "status", status, "wait", wait, "waits", waits, "waited", waited)
}

// Waits is how many waits the service has asked for, and the wall time they
// cover.
func (p *Pacer) Waits() (int, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waits, p.waited
}

// WaitingUntil is when the wait the service last asked for ends, while one is
// running (zero otherwise): the run's progress holds until then.
func (p *Pacer) WaitingUntil() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until.After(p.now()) {
		return p.until
	}
	return time.Time{}
}

// pacedTransport spaces out the requests of a context with a Pacer. It is the remote
// leg's: see Pacer.
type pacedTransport struct{ base http.RoundTripper }

func (t pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if p := pacerOf(req.Context()); p != nil {
		if err := p.wait(req.Context()); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(req)
}
