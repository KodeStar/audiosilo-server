package audible

import (
	"context"
	"errors"
	"fmt"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Bridge Phase 4b: queueing backups (audible.import). The catalog decides what
// to back up and builds each job's params (catalog/audible_import.go); these
// queue the jobs and record each on its title.

// Job priorities: one title asked for in the console comes first, then
// "Back up everything", then what a policy queued after a poll (with the
// scheduled polls).
const (
	priorityBackup    = 0
	priorityBackupAll = -5
	priorityPolicy    = schedulerPriority
)

// Queues is what queueing a backup needs: the titles and the job queue.
type Queues struct {
	Catalog *catalog.Catalog
	Bridge  *bridge.Service
}

// queue queues p's backup at priority for requestedBy (0: the server) and
// records it on the title. The open backup of the title, if there is one, is
// returned instead (created false) and recorded too: a job queued before a
// crash that lost its record is found by its dedupe key.
func (q Queues) queue(ctx context.Context, p bridge.AudibleImportParams, requestedBy int64, priority int) (*bridge.Job, bool, error) {
	job, created, err := q.Bridge.Enqueue(ctx, bridge.AudibleImport(p, requestedBy, priority))
	if err != nil {
		return nil, false, err
	}
	return job, created, q.Catalog.SetAudibleImport(ctx, p.Account, p.ASIN, job.ID)
}

// Backup is the console's "Back up" of one title: the job queued (created), or
// for a preorder pending true and no job (queued once a poll says it is
// available). The catalog's refusals (catalog.RequestAudibleBackup) and the
// queue's (bridge.Enqueue: ErrKindUnavailable, ErrLibraryNotWritable) come
// back as they are.
func (q Queues) Backup(ctx context.Context, account, asin string, requestedBy int64) (job *bridge.Job, created, pending bool, err error) {
	p, pending, err := q.Catalog.RequestAudibleBackup(ctx, account, asin)
	if err != nil || pending {
		return nil, false, pending, err
	}
	job, created, err = q.queue(ctx, *p, requestedBy, priorityBackup)
	return job, created, false, err
}

// BackupAll is "Back up everything" for account: every title
// catalog.AudibleBackupAll names, queued; queued counts the new jobs. It stops
// at the first refusal of the queue (a Bridge that can't take them: none
// will be taken), with what it queued so far.
func (q Queues) BackupAll(ctx context.Context, account string, requestedBy int64) (queued int, err error) {
	all, err := q.Catalog.AudibleBackupAll(ctx, account)
	if err != nil {
		return 0, err
	}
	for _, p := range all {
		if err := ctx.Err(); err != nil {
			return queued, err
		}
		_, created, err := q.queue(ctx, p, requestedBy, priorityBackupAll)
		if err != nil {
			return queued, err
		}
		if created {
			queued++
		}
	}
	return queued, nil
}

// AfterPoll queues what a completed poll of account asks for
// (catalog.AudiblePolicyImports: pending requests now available, and the new
// purchases its policy covers). Nothing while no online Bridge runs
// audible.import (a later poll asks again: the titles stay as they are), and
// a title the queue refuses is skipped.
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
		_, created, err := q.queue(ctx, p, 0, priorityPolicy)
		switch {
		case errors.Is(err, bridge.ErrLibraryNotWritable), errors.Is(err, bridge.ErrKindUnavailable):
			return queued, nil // none will be taken: the next poll asks again
		case err != nil:
			errs = append(errs, fmt.Errorf("back up %s %s: %w", account, p.ASIN, err))
		case created:
			queued++
		}
	}
	return queued, errors.Join(errs...)
}
