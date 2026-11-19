package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/coverage"
)

// foundASINWrite bounds clearFoundASIN's write, which runs inside the lookup
// that found the ASIN: long enough to queue behind a scan's batch, short enough
// that a held write lock costs a lookup little.
const foundASINWrite = 3 * time.Second

// clearFoundASIN is the metadata service's found-ASIN func
// (meta.Service.SetASINFound): an ASIN the unknown-ASIN report holds as
// unknown, found by a lookup, is cleared (catalog.ClearUnknownASIN), so the
// book leaves Health's list now, not at the report's monthly recheck.
//
// The clear outlives the lookup's context, under a short bound of its own: the
// lookup has its answer, and a budget that ends (or a caller that goes away)
// just then would otherwise drop the clear, leaving the ASIN held as unknown,
// where an unknown_asin match run still applies a match found by the title
// beside it (matchrun's asinKnownSince reads this row; server#447). A clear
// that fails is logged: the report's recheck is then what finds it.
func clearFoundASIN(cat *catalog.Catalog, log *slog.Logger) func(ctx context.Context, asin string) {
	return func(ctx context.Context, asin string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), foundASINWrite)
		defer cancel()
		if err := cat.ClearUnknownASIN(ctx, asin); err != nil {
			log.Warn("clearing a found ASIN from the unknown-ASIN report failed", "err", err, "asin", asin)
		}
	}
}

// reportUnknownOn reports whether the daily unknown-ASIN report may run:
// metadata on AND the live metadata.report_unknown, never on a demo server.
func (a *API) reportUnknownOn() bool {
	cfg := a.config()
	return a.metadataOn() && cfg.Metadata.ReportUnknown && !cfg.Demo.Enabled
}

// unknownASINListed reports whether Health lists the unknown_asin kind now: the
// report on, and holding answers (catalog.UnknownASINAnswered). While it is off
// its table is not kept current, and before its first run it holds nothing, so
// a count of 0 or a list of stale rows would read as the books' state
// (server#392). A failed read lists nothing.
func (a *API) unknownASINListed(ctx context.Context) bool {
	if !a.reportUnknownOn() {
		return false
	}
	answered, err := a.cat.UnknownASINAnswered(ctx)
	if err != nil && ctx.Err() == nil {
		a.log.Warn("reading the unknown-ASIN report's answers failed", "err", err)
	}
	return answered
}

// mirrorReady reports whether the report's lookups may start: no mirror
// (remote mode), or its local copy loaded (mirror mode).
func (a *API) mirrorReady() bool { return a.mirror == nil || a.mirror.Ready() }

// reportStatus is the report's last run for the settings envelope
// (metadata.report_status), nil without the job.
func (a *API) reportStatus(ctx context.Context) *coverage.Status {
	if a.coverage == nil {
		return nil
	}
	st, err := a.coverage.Status(ctx)
	if err != nil {
		return nil
	}
	return &st
}
