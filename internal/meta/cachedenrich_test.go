package meta

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// CachedEnrichment answers from memory or the store, stale or not, never the
// upstream; a cached "no match", a cached failure and a row of another format
// are no answer. A fresh stored row warms memory.
func TestCachedEnrichment(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	store := newMemStore()
	svc := NewService(srv.URL, nil)
	svc.SetStore(store)
	ctx := context.Background()

	if env := svc.CachedEnrichment(ctx, "B00FLIJJSY", ""); env != nil {
		t.Fatalf("cold = %+v", env)
	}
	if env := svc.CachedEnrichment(ctx, "", ""); env != nil {
		t.Fatal("no identifiers answered")
	}
	if _, err := svc.Enrich(ctx, "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if env := svc.CachedEnrichment(ctx, " b00flijjsy ", ""); env == nil || env.Work == nil || env.Work.ID == "" {
		t.Fatalf("enriched = %+v", env)
	}
	svc.cache.putMiss(nsISBN.key("9780000000002"), notFoundTTL)
	if env := svc.CachedEnrichment(ctx, "", "9780000000002"); env != nil {
		t.Fatalf("cached no match = %+v", env)
	}
	svc.cache.putError(nsASIN.key("B0FAILED"), errors.New("down"))
	if env := svc.CachedEnrichment(ctx, "B0FAILED", ""); env != nil {
		t.Fatal("a cached failure answered")
	}
	store.put(StoredEntry{Key: nsASIN.key("B0STALE"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"stale","recaps":[{"through":{"chapter":1},"text":"x"}]}}`), Expires: time.Now().Add(-time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0FRESH"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"fresh","characters":[{"id":"c","name":"C","reveal":{"chapter":1}}]}}`), Expires: time.Now().Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0NOMATCH"), Version: storeVersion, Source: srv.URL, Expires: time.Now().Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0OTHER"), Version: storeVersion + 1, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"other"}}`), Expires: time.Now().Add(time.Hour)})
	if env := svc.CachedEnrichment(ctx, "B0STALE", ""); env == nil || env.Work.ID != "stale" || !env.HasRecaps() || env.HasCharacters() {
		t.Fatalf("stale stored row = %+v", env)
	}
	if _, hit, _ := cacheGet[Enrichment](svc.cache, nsASIN.key("B0STALE")); hit {
		t.Error("a stale row warmed memory")
	}
	if env := svc.CachedEnrichment(ctx, "B0FRESH", ""); env == nil || env.Work.ID != "fresh" || !env.HasCharacters() {
		t.Fatalf("fresh stored row = %+v", env)
	}
	if e, hit, err := cacheGet[Enrichment](svc.cache, nsASIN.key("B0FRESH")); !hit || err != nil || e.Work.ID != "fresh" {
		t.Errorf("the fresh row didn't warm memory: %+v, %v, %v", e, hit, err)
	}
	if env := svc.CachedEnrichment(ctx, "B0NOMATCH", ""); env != nil {
		t.Fatalf("stored no match = %+v", env)
	}
	if env := svc.CachedEnrichment(ctx, "B0OTHER", ""); env != nil {
		t.Fatal("a row of another format answered")
	}
	if m.lookupHits.Load() != 1 {
		t.Fatalf("lookups = %d, want only the Enrich's", m.lookupHits.Load())
	}
	var none *Enrichment
	if none.HasCharacters() || none.HasRecaps() {
		t.Fatal("a nil envelope has content")
	}
}
