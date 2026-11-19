package launcher

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/jsonfile"
)

// progressFeedMarkFile is the progress feed's mark in the data folder: where
// the Bridge's progress feed ended (catalog.ProgressFeedEnd) when the server
// last wrote it down. It is outside the database so that a database that went
// back shows: one restored through the console is renewed for by its restore
// (renewProgressFeed), but a backup copied into place by hand, or commits a
// power cut lost, leave the feed its id and a change counter behind the
// cursor the Bridge holds. Once the counter passed that cursor again it would
// page on, past the places written in between (bridge#50). A data folder put
// back whole takes the mark back with it, and is not seen.
const progressFeedMarkFile = "progress-feed.json"

// progressFeedMarkEvery is how often a running server writes the mark when the
// feed's end has moved: what a stop that skips the last write (a kill, a power
// cut) leaves it behind by.
const progressFeedMarkEvery = time.Minute

// progressFeedMark is the mark's content.
type progressFeedMark struct {
	// End is the feed's end: the last number the change counter gave.
	End int64 `json:"end"`
}

// feedMark keeps the progress feed's mark for one run of the server.
type feedMark struct {
	path string
	cat  *catalog.Catalog
	log  *slog.Logger
	// end is what the file holds, -1 for nothing yet, and warned that a
	// failing write was logged (once). The start's goroutine has them until
	// keep, then keep's, then (once that ended) keep's stop.
	end    int64
	warned bool
}

// openProgressFeedMark reads the mark an earlier run left in dataDir and, when
// the database is behind it, renews the Bridge's progress feed
// (catalog.RenewProgressFeedBehind), then writes this database's end as the
// mark. renewed says the feed was renewed already at this start (a restore's),
// so the mark has nothing more to tell. nil when the renewal failed: the mark
// is left as it was, and unwritten for this run, so the next start finds the
// database behind it still and tries again.
func openProgressFeedMark(ctx context.Context, cat *catalog.Catalog, dataDir string, renewed bool, log *slog.Logger) *feedMark {
	m := &feedMark{path: filepath.Join(dataDir, progressFeedMarkFile), cat: cat, log: log, end: -1}
	was, err := jsonfile.Read[progressFeedMark](m.path)
	if err != nil {
		log.Warn("the progress feed's mark can't be read; it is written again", "err", err)
	}
	var mark int64
	if was != nil {
		m.end = was.End
		if !renewed {
			mark = was.End
		}
	}
	end, behind, err := cat.RenewProgressFeedBehind(ctx, mark)
	if err != nil {
		log.Warn("checking the Bridge's progress feed against its mark failed; the next start tries again", "err", err)
		return nil
	}
	if behind {
		log.Warn("the database is behind where this server left it (a backup put in place by hand, or writes a power cut lost): renewed the Bridge's progress feed",
			"end", end, "was", mark)
	}
	m.write(end)
	return m
}

// keep writes the mark every progressFeedMarkEvery while the feed's end moves,
// until stop, which writes it a last time (ctx may have ended by then: a
// server shutting down).
func (m *feedMark) keep(ctx context.Context) (stop func()) {
	keepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressFeedMarkEvery)
		defer ticker.Stop()
		for {
			select {
			case <-keepCtx.Done():
				return
			case <-ticker.C:
				m.save(keepCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		last, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		m.save(last)
	}
}

// save writes the feed's end as the mark, when it moved.
func (m *feedMark) save(ctx context.Context) {
	end, err := m.cat.ProgressFeedEnd(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("reading the progress feed's end for its mark failed", "err", err)
		}
		return
	}
	m.write(end)
}

// write stores end as the mark, unless the file holds it already.
func (m *feedMark) write(end int64) {
	if end == m.end {
		return
	}
	if err := jsonfile.Write(m.path, progressFeedMark{End: end}); err != nil {
		if !m.warned {
			m.warned = true
			m.log.Warn("writing the progress feed's mark failed", "path", m.path, "err", err)
		}
		return
	}
	m.end, m.warned = end, false
}
