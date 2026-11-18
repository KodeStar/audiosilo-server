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

// progressEvery is how often startProgress logs a start-up job's progress.
const progressEvery = 5 * time.Second

// keyPathsProgress is catalog.KeyPaths' progress at the start: the first start
// after the upgrade keys every book. The last count is the "keyed the books'
// paths" line after it.
func keyPathsProgress(log *slog.Logger) func(done, total int) {
	return startProgress(log, "keying the books' paths", progressEvery)
}

// startProgress is the progress of a job the start waits for: msg with the
// counts, at most once every interval while it runs, so a large library's start
// is seen to move. The job's own line follows its last count.
func startProgress(log *slog.Logger, msg string, every time.Duration) func(done, total int) {
	last := time.Now()
	return func(done, total int) {
		if done < total && time.Since(last) >= every {
			last = time.Now()
			log.Info(msg, "done", done, "of", total)
		}
	}
}

// rekeyAtStart runs the start's re-key (catalog.RekeyPathState, server#210)
// after KeyPaths: listeners' rows stored under a track path, a disc folder or
// another spelling of a book's path move to the book, each logged whole in
// path_rekeys first, with a progress line every progressEvery (a library whose
// listeners used an old player has a path a track they played). A repair, so a
// failure never stops the start: it is logged; a run that was stopped runs
// again at the next start, and one that finished tries only the paths that
// failed and the libraries whose folder did not answer (answers), each named
// here when it does.
func rekeyAtStart(ctx context.Context, cat *catalog.Catalog, answers func(catalog.Library) bool, log *slog.Logger) {
	rep, err := cat.RekeyPathState(ctx, answers, startProgress(log, "re-keying listening state to the books' paths", progressEvery))
	if err != nil {
		log.Warn("re-keying listening state to the books' paths failed; trying again at the next start", "err", err)
	}
	for _, r := range rep.Retried {
		if r.Path == "" {
			log.Info("path_rekey retried a library", "library", r.LibraryID)
			continue
		}
		log.Info("path_rekey retried", "library", r.LibraryID, "path", r.Path)
	}
	for _, id := range rep.Unreached {
		log.Warn("path_rekey skipped a library: its folder did not answer; trying again at the next start", "library", id)
	}
	logRekey(log, "re-keyed listening state to the books' paths", rep, slog.LevelInfo)
}

// rootAnswers is the start's re-key's look at each library's folder: the
// scanner's (library.Scanner.RootAvailable: bounded, and an empty folder with
// books indexed under it, an unmounted share, does not answer). With the
// books' counts unread, none does: the re-key waits for the next start.
func rootAnswers(ctx context.Context, cat *catalog.Catalog, scanner *library.Scanner) func(catalog.Library) bool {
	counts, err := cat.CountBooksByLibrary(ctx)
	return func(lib catalog.Library) bool {
		return err == nil && scanner.RootAvailable(lib, counts[lib.ID])
	}
}

// rekeyAfterScan is the scanner's OnRunFinished: the scan-end re-key of the
// library a run just finished (catalog.RekeyLibrary), so a row a client stored
// while its book was not indexed yet moves to it now, then next (the
// notifications). Only after a run that finished (ok or partial); a failure is
// logged and tried again after the next scan. It runs under server, the
// server's context, not the one OnRunFinished is handed (which outlives the
// server, for the run's record): a stop ends it between two paths, the rest
// left for the next scan, rather than waiting for every path.
func rekeyAfterScan(server context.Context, cat *catalog.Catalog, log *slog.Logger,
	next func(context.Context, library.RunReport)) func(context.Context, library.RunReport) {
	return func(ctx context.Context, r library.RunReport) {
		if r.Status == catalog.RunOK || r.Status == catalog.RunPartial {
			rep, err := cat.RekeyLibrary(server, r.Library.ID)
			if err != nil && server.Err() == nil {
				log.Warn("re-keying listening state after a scan failed", "library", r.Library.Name, "err", err)
			}
			// What a run stopped part way moved is logged too. A path a guard
			// keeps is kept at every scan: said at debug level here, at info by
			// the start's run.
			logRekey(log.With("library", r.Library.Name), "re-keyed listening state after a scan", rep, slog.LevelDebug)
		}
		next(ctx, r)
	}
}

// logRekey logs a re-key's report: a line per failed path (warn) and per path
// a guard kept rows on (at skipLevel), then one with the counts (paths moved by
// reason, rows moved and logged, rows the scope guard kept, paths skipped and
// failed) unless it would only repeat skips logged below info.
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
	log.Info(msg, append(args, "rows_logged", rep.Rows, "rows_kept", rep.Kept, "skipped", len(rep.Skipped),
		"failed", len(rep.Failed))...)
}
