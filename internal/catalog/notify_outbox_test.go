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
	ctx := context.Background()
	c := New(storetest.Open(t), time.Now)
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
