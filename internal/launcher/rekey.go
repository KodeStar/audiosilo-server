package launcher

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// keyPathsEvery is how often keyPathsProgress logs a re-key's progress.
const keyPathsEvery = 5 * time.Second

// keyPathsProgress is catalog.KeyPaths' progress at the start: a line at most
// every keyPathsEvery while it runs (the first start after the upgrade keys
// every book), so a large library's start is seen to move. The last count is
// the "keyed the books' paths" line after it.
func keyPathsProgress(log *slog.Logger) func(done, total int) {
	last := time.Now()
	return func(done, total int) {
		if done < total && time.Since(last) >= keyPathsEvery {
			last = time.Now()
			log.Info("keying the books' paths", "done", done, "of", total)
		}
	}
}

// rekeyAtStart runs the start's re-key (catalog.RekeyPathState, server#210)
// after KeyPaths: listeners' rows stored under a track path, a disc folder or
// another spelling of a book's path move to the book, each logged whole in
// path_rekeys first. A repair, so a failure never stops the start: it is
// logged, and the paths that failed are tried at the next start.
func rekeyAtStart(ctx context.Context, cat *catalog.Catalog, log *slog.Logger) {
	rep, err := cat.RekeyPathState(ctx)
	if err != nil {
		log.Warn("re-keying listening state to the books' paths failed; trying again at the next start", "err", err)
	}
	logRekey(log, "re-keyed listening state to the books' paths", rep, slog.LevelInfo)
}

// rekeyAfterScan is the scanner's OnRunFinished: the scan-end re-key of the
// library a run just finished (catalog.RekeyLibrary), so a row a client stored
// while its book was not indexed yet moves to it now, then next (the
// notifications). Only after a run that finished (ok or partial); a failure is
// logged and tried again after the next scan.
func rekeyAfterScan(cat *catalog.Catalog, log *slog.Logger, next func(context.Context, library.RunReport)) func(context.Context, library.RunReport) {
	return func(ctx context.Context, r library.RunReport) {
		if r.Status == catalog.RunOK || r.Status == catalog.RunPartial {
			if rep, err := cat.RekeyLibrary(ctx, r.Library.ID); err != nil {
				log.Warn("re-keying listening state after a scan failed", "library", r.Library.Name, "err", err)
			} else {
				// A path a guard keeps is kept at every scan: said at debug
				// level here, at info by the start's run.
				logRekey(log.With("library", r.Library.Name), "re-keyed listening state after a scan", rep, slog.LevelDebug)
			}
		}
		next(ctx, r)
	}
}

// logRekey logs a re-key's report: a line per failed path (warn) and per path
// a guard kept (at skipLevel), then one with the counts (paths moved by reason,
// rows logged, paths skipped and failed) unless it would only repeat skips
// logged below info.
func logRekey(log *slog.Logger, msg string, rep catalog.RekeyReport, skipLevel slog.Level) {
	ctx := context.Background()
	for _, p := range rep.Failed {
		log.Warn("path_rekey failed", "library", p.LibraryID, "path", p.Path, "to", p.Target, "err", p.Why)
	}
	for _, p := range rep.Skipped {
		log.Log(ctx, skipLevel, "path_rekey skipped", "library", p.LibraryID, "path", p.Path, "to", p.Target, "guard", p.Why)
	}
	if len(rep.Moved) == 0 && len(rep.Failed) == 0 && (len(rep.Skipped) == 0 || skipLevel < slog.LevelInfo) {
		return
	}
	args := []any{}
	for _, reason := range slices.Sorted(maps.Keys(rep.Moved)) {
		args = append(args, "paths_"+reason, rep.Moved[reason])
	}
	log.Info(msg, append(args, "rows_logged", rep.Rows, "skipped", len(rep.Skipped), "failed", len(rep.Failed))...)
}
