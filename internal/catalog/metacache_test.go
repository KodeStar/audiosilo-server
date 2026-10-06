package catalog

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// TestMetaCacheRows: a row round-trips (a "no match" as an empty payload), a
// second put replaces it, and PruneMetaCache keeps the newest rows by write
// time, however they were keyed.
func TestMetaCacheRows(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	c := New(db, func() time.Time { return now })

	if e, err := c.GetMetaCache(ctx, "a:NONE"); err != nil || e != nil {
		t.Fatalf("absent row = %+v, %v", e, err)
	}
	expires := now.Add(24 * time.Hour)
	in := MetaCacheEntry{Key: "a:B0X", Version: 1, Source: "https://meta.example", Payload: []byte(`{"matched":true}`), Expires: expires}
	if err := c.PutMetaCache(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetMetaCache(ctx, "a:B0X")
	if err != nil || got == nil || got.Key != in.Key || got.Version != 1 || got.Source != in.Source ||
		string(got.Payload) != string(in.Payload) || !got.Expires.Equal(expires) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// Replaced in place: a "no match" now.
	if err := c.PutMetaCache(ctx, MetaCacheEntry{Key: "a:B0X", Version: 2, Source: in.Source, Expires: expires}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetMetaCache(ctx, "a:B0X"); got.Version != 2 || len(got.Payload) != 0 {
		t.Fatalf("replaced row = %+v", got)
	}

	// Five more rows, a minute apart; the replaced one (oldest write) and the
	// next two go when three are kept.
	for i := range 5 {
		now = now.Add(time.Minute)
		if err := c.PutMetaCache(ctx, MetaCacheEntry{Key: "w:" + strconv.Itoa(i), Version: 1, Expires: expires}); err != nil {
			t.Fatal(err)
		}
	}
	// Rewriting w:0 makes it the newest.
	now = now.Add(time.Minute)
	if err := c.PutMetaCache(ctx, MetaCacheEntry{Key: "w:0", Version: 1, Expires: expires}); err != nil {
		t.Fatal(err)
	}
	n, err := c.PruneMetaCache(ctx, 3)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v; want 3", n, err)
	}
	for key, kept := range map[string]bool{"a:B0X": false, "w:0": true, "w:1": false, "w:2": false, "w:3": true, "w:4": true} {
		if got, _ := c.GetMetaCache(ctx, key); (got != nil) != kept {
			t.Errorf("%s kept = %v, want %v", key, got != nil, kept)
		}
	}
	if n, err := c.PruneMetaCache(ctx, MetaCacheRows); err != nil || n != 0 {
		t.Fatalf("pruning under the cap = %d, %v", n, err)
	}
}
