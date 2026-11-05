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
