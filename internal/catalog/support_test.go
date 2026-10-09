package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

const aDay = 24 * time.Hour

// supportServer is a catalog whose first account was created at start (as
// auth.CreateUser stamps it: RFC 3339 with nanoseconds), on a clock at start that
// the test moves by setting *now.
func supportServer(t *testing.T, start time.Time) (*Catalog, *time.Time, context.Context, int64) {
	t.Helper()
	now := start
	c := New(storetest.Open(t), func() time.Time { return now })
	ctx := context.Background()
	if _, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	id := addSupportUser(t, c, ctx, "admin", start, false)
	return c, &now, ctx, id
}

func addSupportUser(t *testing.T, c *Catalog, ctx context.Context, name string, created time.Time, demo bool) int64 {
	t.Helper()
	stamp := created.UTC().Format(time.RFC3339Nano)
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, is_demo, created_at, updated_at) VALUES(?, 'x', 'admin', ?, ?, ?)`,
		name, demo, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// finishBooks records n finished books for userID, finished at at.
func finishBooks(t *testing.T, c *Catalog, ctx context.Context, userID int64, n int, at time.Time) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339Nano)
	for i := range n {
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, updated_at, finished_at)
			 VALUES(?, 1, ?, 0, 100, 1, ?, ?)`,
			userID, fmt.Sprintf("book-%d-%s", i, stamp), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func wantDue(t *testing.T, c *Catalog, ctx context.Context, want bool, why string) {
	t.Helper()
	got, err := c.SupportCardDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: SupportCardDue = %v, want %v", why, got, want)
	}
}

var supportStart = time.Date(2026, 1, 10, 9, 30, 0, 123456789, time.UTC)

// The card waits for a month from the earliest real account: a later account, a
// demo one or a row whose date can't be read doesn't make the server newer or older.
func TestSupportCardWaitsForAMonth(t *testing.T) {
	c, now, ctx, _ := supportServer(t, supportStart)
	wantDue(t, c, ctx, false, "first run")
	addSupportUser(t, c, ctx, "late", supportStart.Add(25*aDay), false)
	addSupportUser(t, c, ctx, "demo", supportStart.Add(-90*aDay), true)
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES('odd', 'x', 'user', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	*now = supportStart.Add(SupportAfterDays*aDay - time.Minute)
	wantDue(t, c, ctx, false, "a minute short of a month")
	*now = supportStart.Add(SupportAfterDays * aDay)
	wantDue(t, c, ctx, true, "a month in")
}

func TestSupportCardNeverWithoutAccounts(t *testing.T) {
	now := supportStart.Add(365 * aDay)
	c := New(storetest.Open(t), func() time.Time { return now })
	ctx := context.Background()
	wantDue(t, c, ctx, false, "no accounts yet")
	// A demo account is a visitor, not the server's first use.
	addSupportUser(t, c, ctx, "demo", supportStart, true)
	wantDue(t, c, ctx, false, "only a demo account")
}

func TestSupportCardAfterFinishedBooks(t *testing.T) {
	c, now, ctx, admin := supportServer(t, supportStart)
	finishBooks(t, c, ctx, admin, SupportAfterFinished, supportStart.Add(time.Hour))
	wantDue(t, c, ctx, false, "first run, however many finishes")
	*now = now.Add(SupportMinDays * aDay)
	wantDue(t, c, ctx, true, "a week in with enough finishes")
}

func TestSupportCardFinishesCountedHere(t *testing.T) {
	c, now, ctx, admin := supportServer(t, supportStart)
	*now = now.Add(10 * aDay)
	// Imported from another server: finished before this one existed.
	finishBooks(t, c, ctx, admin, 50, supportStart.Add(-200*aDay))
	// A demo visitor's finishes don't count either.
	demo := addSupportUser(t, c, ctx, "demo", supportStart.Add(aDay), true)
	finishBooks(t, c, ctx, demo, SupportAfterFinished, supportStart.Add(2*aDay))
	finishBooks(t, c, ctx, admin, SupportAfterFinished-1, supportStart.Add(3*aDay))
	wantDue(t, c, ctx, false, "one finish short")
	finishBooks(t, c, ctx, admin, 1, supportStart.Add(4*aDay))
	wantDue(t, c, ctx, true, "enough finishes here")
}

func TestSupportCardDonatedHidesForGood(t *testing.T) {
	c, now, ctx, _ := supportServer(t, supportStart)
	*now = now.Add(SupportAfterDays * aDay)
	wantDue(t, c, ctx, true, "due")
	res, err := c.SetSupportChoice(ctx, SupportDonated)
	if err != nil || !res.Changed || !res.Until.IsZero() {
		t.Fatalf("donated = %+v, %v", res, err)
	}
	wantDue(t, c, ctx, false, "after donating")
	*now = now.Add(10 * 365 * aDay)
	wantDue(t, c, ctx, false, "years after donating")
	// A later "Not now" never turns "for good" into a snooze.
	res, err = c.SetSupportChoice(ctx, SupportSnoozed)
	if err != nil || res.Changed || !res.Until.IsZero() {
		t.Fatalf("snooze after donating = %+v, %v", res, err)
	}
	*now = now.Add(365 * aDay)
	wantDue(t, c, ctx, false, "a snooze after donating")
}

func TestSupportCardSnoozeReturns(t *testing.T) {
	c, now, ctx, _ := supportServer(t, supportStart)
	*now = now.Add(SupportAfterDays * aDay)
	res, err := c.SetSupportChoice(ctx, SupportSnoozed)
	if err != nil || !res.Changed {
		t.Fatalf("snooze = %+v, %v", res, err)
	}
	if want := now.AddDate(0, SupportSnoozeMonths, 0); !res.Until.Equal(want) {
		t.Fatalf("until = %v, want %v", res.Until, want)
	}
	wantDue(t, c, ctx, false, "snoozed")
	*now = now.Add(res.Until.Sub(*now) - time.Second)
	wantDue(t, c, ctx, false, "a second before the snooze ends")
	*now = now.Add(time.Second)
	wantDue(t, c, ctx, true, "the snooze ended")

	// Snoozed again, then donated: donating replaces a snooze.
	if _, err := c.SetSupportChoice(ctx, SupportSnoozed); err != nil {
		t.Fatal(err)
	}
	if res, err := c.SetSupportChoice(ctx, SupportDonated); err != nil || !res.Changed {
		t.Fatalf("donate after snooze = %+v, %v", res, err)
	}
	*now = now.Add(365 * aDay)
	wantDue(t, c, ctx, false, "donated after a snooze")
}

func TestSupportCardOddStoredState(t *testing.T) {
	c, now, ctx, _ := supportServer(t, supportStart)
	*now = now.Add(SupportAfterDays * aDay)
	for _, value := range []string{`not json`, `{"choice":"snoozed","until":"soon"}`, `{"choice":"maybe"}`} {
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO server_state(key, value, updated_at) VALUES(?, ?, 't')
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, supportKey, value); err != nil {
			t.Fatal(err)
		}
		wantDue(t, c, ctx, true, value)
	}
	if _, err := c.SetSupportChoice(ctx, "later"); err != ErrInvalidSupportChoice {
		t.Fatalf("unknown choice: err = %v", err)
	}
}
