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
