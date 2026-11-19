package audible

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Bridge Phase 4b: queueing backups (audible.import). The catalog decides what
// to back up and builds each job's params (catalog/audible_import.go); these
// queue the jobs and record each on its title.

// Queues is what queueing a backup needs: the titles and the job queue.
type Queues struct {
	Catalog *catalog.Catalog
	Bridge  *bridge.Service
}

// queue queues p's backup at priority for requestedBy (0: the server) and
// records it on the title in the queue's transaction (bridge.EnqueueAll, a
// batch of one): a job is never queued without its title's record, nor the
// record written for a job that didn't commit. The open backup of the title,
// if there is one, is recorded instead (created false): one a concurrent ask
// queued first, or one from before the two committed together.
func (q Queues) queue(ctx context.Context, p bridge.AudibleImportParams, requestedBy int64, priority int) (jobID int64, created bool, err error) {
	n, err := q.Bridge.EnqueueAll(ctx, []bridge.NewJob{bridge.AudibleImport(p, requestedBy, priority)},
		func(tx *sql.Tx, _ int, id int64) error {
			jobID = id
			return q.Catalog.SetAudibleImportTx(ctx, tx, p.Account, p.ASIN, id)
		})
	if err != nil {
		return 0, false, err
	}
	return jobID, n > 0, nil
}

// Backup is the console's "Back up" of one title: its queued job's id
// (created, or the open one a concurrent ask queued first), or for a preorder
// pending true and no job (queued once a poll says it is available). The
// catalog's refusals (catalog.RequestAudibleBackup) and the queue's
// (bridge.EnqueueAll: ErrKindUnavailable, ErrLibraryNotWritable) come back as
// they are.
func (q Queues) Backup(ctx context.Context, account, asin string, requestedBy int64) (jobID int64, created, pending bool, err error) {
	p, pending, err := q.Catalog.RequestAudibleBackup(ctx, account, asin)
	if err != nil || pending {
		return 0, false, pending, err
	}
	jobID, created, err = q.queue(ctx, *p, requestedBy, bridge.PriorityAsked)
	return jobID, created, false, err
}

// BackupAll is "Back up everything" for account: every title
// catalog.AudibleBackupAll names, queued and recorded on its title in one
// transaction (bridge.EnqueueAll); queued counts the new jobs. A title whose
// stored destination no longer validates (one saved before a rule tightened:
// bridge.CheckAudibleDestPath) is skipped and counted in skipped, so it
// doesn't stop the batch; only that title-level reason is skipped, so an
// account-level one (its template) still comes back as the queue's refusal.
// A refusal of the queue (a Bridge that can't take them: none will be taken)
// stops it at the title refused, with the titles before it queued; any other
// failure queues none.
func (q Queues) BackupAll(ctx context.Context, account string, requestedBy int64) (queued, skipped int, err error) {
	all, err := q.Catalog.AudibleBackupAll(ctx, account)
	if err != nil {
		return 0, 0, err
	}
	valid := all[:0]
	jobs := make([]bridge.NewJob, 0, len(all))
	for _, p := range all {
		if bridge.CheckAudibleDestPath(p.DestPath) != nil {
			skipped++
			continue
		}
		valid = append(valid, p)
		jobs = append(jobs, bridge.AudibleImport(p, requestedBy, bridge.PriorityBulk))
	}
	queued, err = q.Bridge.EnqueueAll(ctx, jobs, func(tx *sql.Tx, i int, jobID int64) error {
		return q.Catalog.SetAudibleImportTx(ctx, tx, valid[i].Account, valid[i].ASIN, jobID)
	})
	return queued, skipped, err
}

// AfterPoll queues what a completed poll of account asks for
// (catalog.AudiblePolicyImports: pending requests now available, at the
// console's priority, and the new purchases its policy covers). Nothing while
// no online Bridge runs audible.import (a later poll asks again: the titles
// stay as they are), and a title the queue refuses is skipped.
func (q Queues) AfterPoll(ctx context.Context, account string) (queued int, err error) {
	h, err := q.Bridge.Online(ctx)
	if err != nil || h == nil || !h.Offers(bridge.KindAudibleImport) {
		return 0, err
	}
	want, err := q.Catalog.AudiblePolicyImports(ctx, account)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, p := range want {
		if err := ctx.Err(); err != nil {
			return queued, err
		}
		priority := bridge.PriorityScheduled
		if p.Requested {
			priority = bridge.PriorityAsked
		}
		_, created, err := q.queue(ctx, p.AudibleImportParams, 0, priority)
		if created {
			queued++
		}
		switch {
		case errors.Is(err, bridge.ErrLibraryNotWritable), errors.Is(err, bridge.ErrKindUnavailable):
			return queued, nil // none will be taken: the next poll asks again
		case err != nil:
			errs = append(errs, fmt.Errorf("back up %s %s: %w", account, p.ASIN, err))
		}
	}
	return queued, errors.Join(errs...)
}
