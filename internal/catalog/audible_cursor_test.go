package catalog

import (
	"encoding/base64"
	"errors"
	"testing"
)

// A progress cursor names a row: base64url JSON that is null, {}, or misses
// the row's number or ASIN is refused, never read as the start (bridge#50).
func TestDecodeProgressCursor(t *testing.T) {
	t.Parallel()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{enc("null"), enc("{}"), enc(`{"q":0,"s":"B00"}`), enc(`{"q":3}`), enc(`{"s":"B00"}`),
		enc(`{"q":-1,"s":"B00"}`), enc("[]"), "not.base64"} {
		if _, err := decodeProgressCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%q: err = %v, want ErrInvalidCursor", bad, err)
		}
	}
	for s, want := range map[string]progressCursor{
		"":                               {},
		enc(`{"q":3,"s":"B00"}`):         {Seq: 3, ASIN: "B00"},
		enc(`{"q":3,"s":"B00","f":"x"}`): {Seq: 3, ASIN: "B00", Feed: "x"},
	} {
		if got, err := decodeProgressCursor(s); err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", s, got, err, want)
		}
	}
}

// The feed's id is made once and kept; a renewal (a backup restored) makes a
// new one, once for that restore.
func TestProgressFeedID(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	first, err := f.c.progressFeedID(f.ctx)
	if err != nil || first == "" {
		t.Fatalf("id = %q, %v", first, err)
	}
	if again, err := f.c.progressFeedID(f.ctx); err != nil || again != first {
		t.Fatalf("id again = %q, %v; want %q", again, err, first)
	}
	if renewed, err := f.c.RenewProgressFeed(f.ctx, "a restore"); err != nil || !renewed {
		t.Fatalf("renewal = %v, %v", renewed, err)
	}
	second, err := f.c.progressFeedID(f.ctx)
	if err != nil || second == "" || second == first {
		t.Fatalf("renewed id = %q, %v; want a new one", second, err)
	}
	if renewed, err := f.c.RenewProgressFeed(f.ctx, "a restore"); err != nil || renewed {
		t.Fatalf("renewal again for the same restore = %v, %v; want none", renewed, err)
	}
	if kept, err := f.c.progressFeedID(f.ctx); err != nil || kept != second {
		t.Fatalf("id after the same restore again = %q, %v; want %q", kept, err, second)
	}
	if renewed, err := f.c.RenewProgressFeed(f.ctx, "another restore"); err != nil || !renewed {
		t.Fatalf("renewal for another restore = %v, %v", renewed, err)
	}
	if third, err := f.c.progressFeedID(f.ctx); err != nil || third == "" || third == second {
		t.Fatalf("id after another restore = %q, %v; want a new one", third, err)
	}
}

// A database that went back with no restore recorded (its change counter is
// below the mark the launcher kept outside it) gets a new feed id; one at or
// past the mark keeps its id.
func TestRenewProgressFeedBehind(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE progress_change_counter SET seq = 7 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	first, err := f.c.progressFeedID(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mark := range []int64{0, 6, 7} {
		if end, renewed, err := f.c.RenewProgressFeedBehind(f.ctx, mark); err != nil || renewed || end != 7 {
			t.Fatalf("mark %d: end %d, renewed %v, %v; want 7 and no renewal", mark, end, renewed, err)
		}
	}
	if kept, err := f.c.progressFeedID(f.ctx); err != nil || kept != first {
		t.Fatalf("id = %q, %v; want %q kept", kept, err, first)
	}
	if end, renewed, err := f.c.RenewProgressFeedBehind(f.ctx, 8); err != nil || !renewed || end != 7 {
		t.Fatalf("mark 8: end %d, renewed %v, %v; want 7 and a renewal", end, renewed, err)
	}
	if second, err := f.c.progressFeedID(f.ctx); err != nil || second == "" || second == first {
		t.Fatalf("id behind the mark = %q, %v; want a new one", second, err)
	}
}

// Without refuseStale (a v0.6.0 Bridge), a cursor past the feed's end reads as
// the feed's end, a cursor after every row of the counter's last number; in a
// feed no row was ever written to (the counter at 0) it stays the place it
// names. With refuseStale it is refused.
func TestProgressFromPastEnd(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	feed, err := f.c.progressFeedID(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ahead := progressCursor{Seq: 9, ASIN: "B000000001", Feed: feed}
	if _, err := f.c.progressFrom(f.ctx, ahead.encode(), feed, true); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("refused: err = %v, want ErrInvalidCursor", err)
	}
	if from, err := f.c.progressFrom(f.ctx, ahead.encode(), feed, false); err != nil || from != ahead {
		t.Fatalf("counter at 0 = %+v, %v; want %+v as it is", from, err, ahead)
	}
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE progress_change_counter SET seq = 7 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	want := progressCursor{Seq: 7, ASIN: progressEndASIN, Feed: feed}
	if from, err := f.c.progressFrom(f.ctx, ahead.encode(), feed, false); err != nil || from != want {
		t.Fatalf("counter at 7 = %+v, %v; want %+v", from, err, want)
	}
	if from, err := f.c.progressFrom(f.ctx, want.encode(), feed, true); err != nil || from != want {
		t.Fatalf("the feed's end, refusing = %+v, %v; want it read as itself", from, err)
	}
	if !(progressEndASIN > "ZZZZZZZZZZ" && progressEndASIN > "9999999999") {
		t.Fatalf("progressEndASIN %q sorts before an ASIN", progressEndASIN)
	}
}
