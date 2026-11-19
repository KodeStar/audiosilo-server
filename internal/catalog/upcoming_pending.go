package catalog

import (
	"cmp"
	"context"
	"slices"
)

// upcomingPendingKey is the upcoming job's pending books in server_state.
const upcomingPendingKey = "upcoming_pending"

// UpcomingPending is the books whose work a step-1-alone run of the upcoming job
// found new or changed, waiting for the next full run's series step
// (upcoming.Runner), so a restart in between doesn't lose them (server#447): their
// series would otherwise not be looked at again until the monthly refresh. Kept
// by path, like the job's other rows, so a rebuilt index keeps them; nil for none.
func (c *Catalog) UpcomingPending(ctx context.Context) ([]Ref, error) {
	return getServerState[[]Ref](ctx, c.db, upcomingPendingKey)
}

// SaveUpcomingPending replaces the upcoming job's pending books with refs,
// sorting refs in place (a stable order).
func (c *Catalog) SaveUpcomingPending(ctx context.Context, refs []Ref) error {
	slices.SortFunc(refs, func(a, b Ref) int {
		return cmp.Or(cmp.Compare(a.LibraryID, b.LibraryID), cmp.Compare(a.Path, b.Path))
	})
	return putServerState(ctx, c.db, upcomingPendingKey, refs, c.stamp())
}
