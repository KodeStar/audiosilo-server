package api

import (
	"context"

	"github.com/kodestar/audiosilo-server/internal/coverage"
)

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
