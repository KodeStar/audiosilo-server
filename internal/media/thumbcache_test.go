package media

import (
	"strings"
	"testing"
)

func TestThumbCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Repeat("x", 100))
	cost := entryCost("a", data)
	c := NewThumbCache(cost * 2)
	c.Put("a", data)
	c.Put("b", data)
	if _, ok := c.Get("a"); !ok { // a is now the most recently used
		t.Fatal("a should be cached")
	}
	c.Put("c", data) // over budget: evicts b, the least recently used
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should be cached", k)
		}
	}
}

func TestThumbCacheRemembersNoArt(t *testing.T) {
	t.Parallel()
	c := NewThumbCache(1 << 10)
	c.Put("none", nil)
	data, ok := c.Get("none")
	if !ok || data != nil {
		t.Fatalf("got %q, %v; want a cached no-art value", data, ok)
	}
}

func TestThumbCacheReplaceKeepsAccounting(t *testing.T) {
	t.Parallel()
	c := NewThumbCache(1 << 10)
	small := []byte("small")
	c.Put("k", []byte(strings.Repeat("x", 500)))
	c.Put("k", small)
	if c.used != entryCost("k", small) {
		t.Fatalf("used = %d after a replace, want %d", c.used, entryCost("k", small))
	}
	if got, _ := c.Get("k"); string(got) != "small" {
		t.Fatalf("got %q after a replace, want %q", got, small)
	}
}

func TestThumbCacheSkipsOversizedEntry(t *testing.T) {
	t.Parallel()
	c := NewThumbCache(100)
	c.Put("big", []byte(strings.Repeat("x", 200)))
	if _, ok := c.Get("big"); ok {
		t.Fatal("an entry larger than the cache should not be stored")
	}
}
