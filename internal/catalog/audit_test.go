package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestAuditRetention(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	c := New(db, func() time.Time { return now })

	record := func(at time.Time, action string) {
		now = at
		if err := c.RecordAudit(ctx, AuditEvent{Action: action, Target: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	record(time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), "user.old")
	record(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "user.new")
	n, err := c.PruneAudit(ctx, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).Add(-AuditRetention))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	events, _, err := c.ListAudit(ctx, AuditFilter{})
	if err != nil || len(events) != 1 || events[0].Action != "user.new" || events[0].Via != ViaSession {
		t.Fatalf("left = %+v, %v", events, err)
	}
	// LIKE wildcards in a search or area are literal.
	if got, _, _ := c.ListAudit(ctx, AuditFilter{Query: "%"}); len(got) != 0 {
		t.Fatalf("%% matched %+v", got)
	}
	if got, _, _ := c.ListAudit(ctx, AuditFilter{Area: "_ser"}); len(got) != 0 {
		t.Fatalf("_ matched %+v", got)
	}

	if _, err := c.RecordServerEvent(ctx, "book_added", nil, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(100 * 24 * time.Hour)
	if n, err := c.PruneServerEvents(ctx, now.Add(-ServerEventRetention)); err != nil || n != 1 {
		t.Fatalf("pruned events %d, %v", n, err)
	}
}

func TestPageSizesClamp(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	c := New(db, time.Now)
	for range 120 {
		if _, err := c.RecordServerEvent(ctx, "book_added", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if events, _, _ := c.ListServerEvents(ctx, 0, 500, ""); len(events) != 100 {
		t.Fatalf("a big limit gave %d events, want the most (100)", len(events))
	}
	if events, _, _ := c.ListServerEvents(ctx, 0, 0, ""); len(events) != 20 {
		t.Fatalf("no limit gave %d events, want the default (20)", len(events))
	}
}
