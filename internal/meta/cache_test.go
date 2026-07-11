package meta

import (
	"strconv"
	"testing"
	"time"
)

func TestCacheGetMissAndExpiry(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	if _, ok := c.get("absent"); ok {
		t.Fatal("expected a miss for an absent key")
	}
	c.putResult("k", &Enrichment{Matched: true}, positiveTTL)
	if _, ok := c.get("k"); !ok {
		t.Fatal("expected a hit right after put")
	}
	clk.advance(positiveTTL + time.Second)
	if _, ok := c.get("k"); ok {
		t.Fatal("expected an expired entry to miss")
	}
}

func TestCacheEvictsUnderCap(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newCache(clk.now)

	// Fill past the cap; the map must never exceed it.
	for i := 0; i < cacheCap+500; i++ {
		c.putResult("k"+strconv.Itoa(i), &Enrichment{Matched: true}, positiveTTL)
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
	c.putResult("fresh", &Enrichment{Matched: true}, positiveTTL)
	if _, ok := c.get("fresh"); !ok {
		t.Fatal("fresh entry should be retained")
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > cacheCap {
		t.Fatalf("cache exceeded its cap after eviction: %d", n)
	}
}

var errTest = errTestType("boom")

type errTestType string

func (e errTestType) Error() string { return string(e) }
