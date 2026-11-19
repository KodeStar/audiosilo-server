package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// An event is queued in the outbox, in its own transaction, for the enabled
// destinations subscribed to its kind only; a delivery is claimed once, ends
// once, and its rows go with the event's retention (server#465).
func TestNotifyOutbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	c := New(db, func() time.Time { return now })
	add := func(enabled bool, events ...string) int64 {
		t.Helper()
		tg, err := c.CreateNotifyTarget(ctx, NotifyTarget{Kind: "webhook", Name: "w", URL: "http://x", Enabled: enabled, Events: events})
		if err != nil {
			t.Fatal(err)
		}
		return tg.ID
	}
	subscribed := add(true, "book_added", "scan_failed")
	add(true, "scan_failed")              // not subscribed to book_added
	add(false, "book_added")              // switched off
	other := add(true, "book_added_more") // a kind sharing a prefix is not the kind

	ev, err := c.RecordServerEvent(ctx, "book_added", map[string]any{"count": 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	owed, err := c.PendingDeliveries(ctx, ev.ID)
	if err != nil || len(owed) != 1 || owed[0].TargetID != subscribed || owed[0].Attempts != 0 ||
		owed[0].Event.Kind != "book_added" || owed[0].Event.Data["count"] != float64(1) {
		t.Fatalf("owed = %+v, %v; want one delivery to %d (not %d)", owed, err, subscribed, other)
	}
	d := owed[0]

	// Two claims at the same attempt: one wins.
	if mine, err := c.ClaimDelivery(ctx, d.ID, 0); err != nil || !mine {
		t.Fatalf("first claim = %v, %v", mine, err)
	}
	if mine, _ := c.ClaimDelivery(ctx, d.ID, 0); mine {
		t.Fatal("a second claim at the same attempt won")
	}
	if all, _ := c.PendingDeliveries(ctx, 0); len(all) != 1 || all[0].Attempts != 1 {
		t.Fatalf("after a claim = %+v", all)
	}
	if err := c.FinishDelivery(ctx, d.ID, DeliveryDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if all, _ := c.PendingDeliveries(ctx, 0); len(all) != 0 {
		t.Fatalf("a delivered row is still owed: %+v", all)
	}
	if mine, _ := c.ClaimDelivery(ctx, d.ID, 1); mine {
		t.Fatal("a delivered row was claimed")
	}

	// An event recorded in a transaction that rolls back leaves no row owed.
	errRollback := errors.New("rollback")
	err = db.WithTx(ctx, "test", func(tx *sql.Tx) error {
		if _, err := c.onSnapshot(tx).RecordServerEvent(ctx, "book_added", nil, ""); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if all, _ := c.PendingDeliveries(ctx, 0); len(all) != 0 {
		t.Fatalf("a rolled-back event is owed: %+v", all)
	}

	// A pending row older than the retention is no longer owed, and is pruned.
	if _, err := c.RecordServerEvent(ctx, "scan_failed", nil, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(NotifyOutboxRetention + time.Hour)
	if all, _ := c.PendingDeliveries(ctx, 0); len(all) != 0 {
		t.Fatalf("a stale delivery is owed: %+v", all)
	}
	if n, err := c.PruneNotifyOutbox(ctx, now.Add(-NotifyOutboxRetention)); err != nil || n != 3 {
		t.Fatalf("pruned %d, %v; want the three rows", n, err)
	}
}

// A destination's deletion takes its owed deliveries with it.
func TestNotifyOutboxFollowsTheDestination(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	tg, err := c.CreateNotifyTarget(ctx, NotifyTarget{Kind: "webhook", Name: "w", URL: "http://x", Enabled: true, Events: []string{"book_added"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordServerEvent(ctx, "book_added", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNotifyTarget(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := c.PendingDeliveries(ctx, 0); len(all) != 0 {
		t.Fatalf("owed to a deleted destination: %+v", all)
	}
}

// A destination whose events aren't JSON (a row edited by hand) is owed
// nothing, as scanTarget reads it, and the event is recorded all the same.
func TestNotifyOutboxPassesOverUnreadableEvents(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	tg, err := c.CreateNotifyTarget(ctx, NotifyTarget{Kind: "webhook", Name: "w", URL: "http://x", Enabled: true, Events: []string{"book_added"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE notification_targets SET events = 'book_added' WHERE id = ?`, tg.ID); err != nil {
		t.Fatal(err)
	}
	ev, err := c.RecordServerEvent(ctx, "book_added", nil, "")
	if err != nil {
		t.Fatalf("recording beside unreadable events: %v", err)
	}
	if owed, err := c.PendingDeliveries(ctx, ev.ID); err != nil || len(owed) != 0 {
		t.Fatalf("owed = %+v, %v; want none", owed, err)
	}
}

// A destination is owed its oldest event first, whatever the others are owed,
// and nothing once its rows have ended.
func TestNextDeliveryIsTheOldestEventOwed(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	add := func(events ...string) int64 {
		t.Helper()
		tg, err := c.CreateNotifyTarget(ctx, NotifyTarget{Kind: "webhook", Name: "w", URL: "http://x", Enabled: true, Events: events})
		if err != nil {
			t.Fatal(err)
		}
		return tg.ID
	}
	both, late := add("scan_failed", "book_added"), add("book_added")
	var ids []int64
	for _, kind := range []string{"scan_failed", "book_added", "book_added"} {
		ev, err := c.RecordServerEvent(ctx, kind, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	next := func(target int64) int64 {
		t.Helper()
		d, ok, err := c.NextDelivery(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return 0
		}
		if d.TargetID != target {
			t.Fatalf("a delivery owed to %d was read for %d", d.TargetID, target)
		}
		if err := c.FinishDelivery(ctx, d.ID, DeliveryDelivered, ""); err != nil {
			t.Fatal(err)
		}
		return d.Event.ID
	}
	if a, b := next(late), next(late); a != ids[1] || b != ids[2] {
		t.Fatalf("the later destination was owed %d then %d, want %d then %d", a, b, ids[1], ids[2])
	}
	for i, want := range append(ids, 0) {
		if got := next(both); got != want {
			t.Fatalf("delivery %d to the first destination was event %d, want %d", i, got, want)
		}
	}
}
