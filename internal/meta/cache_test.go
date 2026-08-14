package meta

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestCacheGetMissAndExpiry(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	if _, hit, _ := c.getEnrichment("absent"); hit {
		t.Fatal("expected a miss for an absent key")
	}
	c.putEnrichment("k", &Enrichment{Matched: true}, positiveTTL)
	if _, hit, err := c.getEnrichment("k"); !hit || err != nil {
		t.Fatalf("expected a hit right after put, got hit=%v err=%v", hit, err)
	}
	clk.advance(positiveTTL + time.Second)
	if _, hit, _ := c.getEnrichment("k"); hit {
		t.Fatal("expected an expired entry to miss")
	}
}

// TestCacheTypedAccessorsIsolatePayloads: the two lookup kinds share one map, so
// the payload's meaning must come from its TYPE, not from a key-prefix
// convention. A cached "no match" reads as ErrNotFound only through the matching
// accessor; the other one must report a MISS (an extra upstream fetch), never a
// phantom negative.
func TestCacheTypedAccessorsIsolatePayloads(t *testing.T) {
	c := newCache(nil)

	c.putEnrichment("e", &Enrichment{Matched: true}, positiveTTL)
	c.putWork("w", &MetaWork{ID: "the-martian"}, positiveTTL)

	if work, hit, _ := c.getWork("e"); hit || work != nil {
		t.Fatal("an enrichment entry must not satisfy a work read")
	}
	if res, hit, _ := c.getEnrichment("w"); hit || res != nil {
		t.Fatal("a work entry must not satisfy an enrichment read")
	}
	if res, hit, err := c.getEnrichment("e"); !hit || err != nil || res == nil {
		t.Fatalf("enrichment hit = %v/%v/%v", res, hit, err)
	}
	if work, hit, err := c.getWork("w"); !hit || err != nil || work == nil {
		t.Fatalf("work hit = %v/%v/%v", work, hit, err)
	}

	// A "no match" marker: ErrNotFound through either accessor (it carries no
	// payload, so there is no type to disagree with) - but only while live.
	c.putMiss("miss", notFoundTTL)
	if _, hit, err := c.getEnrichment("miss"); !hit || !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached miss = hit %v, err %v", hit, err)
	}
	// A cached transport error surfaces as itself, not as a not-found.
	c.putError("boom", errTest)
	if _, hit, err := c.getWork("boom"); !hit || !errors.Is(err, errTest) {
		t.Fatalf("cached error = hit %v, err %v", hit, err)
	}
}

// TestCacheKeyspacesDistinct: every key is minted through keyspace.key, and the
// three spaces must not collide for the same id.
func TestCacheKeyspacesDistinct(t *testing.T) {
	const id = "the-martian"
	keys := map[string]bool{nsASIN.key(id): true, nsISBN.key(id): true, nsWork.key(id): true}
	if len(keys) != 3 {
		t.Fatalf("key spaces collide: %v", keys)
	}
	if got := nsWork.key(id); got != "w:"+id {
		t.Fatalf("work key = %q", got)
	}
}

func TestCacheEvictsUnderCap(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	// Fill past the cap; the map must never exceed it.
	for i := 0; i < cacheCap+500; i++ {
		c.putEnrichment("k"+strconv.Itoa(i), &Enrichment{Matched: true}, positiveTTL)
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > cacheCap {
		t.Fatalf("cache exceeded its cap: %d > %d", n, cacheCap)
	}
}

func TestCacheEvictsExpiredFirst(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	// Fill to capacity with short-lived (error-TTL) entries, then expire them.
	for i := 0; i < cacheCap; i++ {
		c.putError("old"+strconv.Itoa(i), errTest)
	}
	clk.advance(errorTTL + time.Second)
	// A fresh insert triggers eviction, which drops the expired entries first.
	c.putEnrichment("fresh", &Enrichment{Matched: true}, positiveTTL)
	if _, hit, _ := c.getEnrichment("fresh"); !hit {
		t.Fatal("fresh entry should be retained")
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > cacheCap {
		t.Fatalf("cache exceeded its cap after eviction: %d", n)
	}
}

// TestWorkFloodCannotEvictEnrichment: the work id is the only freely enumerable
// cache key (GET /meta/work?id=), so a flood of distinct ids must stay inside
// its own key space. Before the quota, cacheCap distinct work ids flushed every
// a:/i: entry and turned each book view back into a full upstream fan-out.
func TestWorkFloodCannotEvictEnrichment(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	// A handful of enrichment entries - the state a flood must not be able to
	// touch (they are what keeps a warm library's book views cheap).
	const enrichments = 50
	for i := 0; i < enrichments; i++ {
		c.putEnrichment(nsASIN.key("B0"+strconv.Itoa(i)), &Enrichment{Matched: true}, positiveTTL)
	}
	// Flood well past BOTH the work quota and the global cap.
	for i := 0; i < cacheCap*2; i++ {
		c.putWork(nsWork.key("w"+strconv.Itoa(i)), &MetaWork{ID: "w" + strconv.Itoa(i)}, positiveTTL)
	}

	for i := 0; i < enrichments; i++ {
		if _, hit, err := c.getEnrichment(nsASIN.key("B0" + strconv.Itoa(i))); !hit || err != nil {
			t.Fatalf("enrichment %d evicted by a work flood (hit=%v err=%v)", i, hit, err)
		}
	}
	c.mu.Lock()
	total := len(c.m)
	works := 0
	for k := range c.m {
		if nsWork.owns(k) {
			works++
		}
	}
	c.mu.Unlock()
	if works > maxWorkEntries {
		t.Fatalf("work entries = %d, want <= %d", works, maxWorkEntries)
	}
	if total > cacheCap {
		t.Fatalf("cache exceeded its cap: %d > %d", total, cacheCap)
	}
	// The quota bounds the work space without disabling it: the most recent
	// work entry is still cached.
	last := nsWork.key("w" + strconv.Itoa(cacheCap*2-1))
	if _, hit, err := c.getWork(last); !hit || err != nil {
		t.Fatalf("newest work entry should be cached (hit=%v err=%v)", hit, err)
	}
}

var errTest = errTestType("boom")

type errTestType string

func (e errTestType) Error() string { return string(e) }
