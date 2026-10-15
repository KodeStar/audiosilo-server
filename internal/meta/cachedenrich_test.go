package meta

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// CachedEnrichment answers from memory or the store, stale or not, never the
// upstream; a cached failure and a row of another format are no answer.
func TestCachedEnrichment(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	store := newMemStore()
	svc := NewService(srv.URL, nil)
	svc.SetStore(store)
	ctx := context.Background()

	if env, ok := svc.CachedEnrichment(ctx, "B00FLIJJSY", ""); ok || env != nil {
		t.Fatalf("cold = %+v, %v", env, ok)
	}
	if _, ok := svc.CachedEnrichment(ctx, "", ""); ok {
		t.Fatal("no identifiers answered")
	}
	if _, err := svc.Enrich(ctx, "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if env, ok := svc.CachedEnrichment(ctx, " b00flijjsy ", ""); !ok || env == nil || env.Work == nil || env.Work.ID == "" {
		t.Fatalf("enriched = %+v, %v", env, ok)
	}
	svc.cache.putMiss(nsISBN.key("9780000000002"), notFoundTTL)
	if env, ok := svc.CachedEnrichment(ctx, "", "9780000000002"); !ok || env != nil {
		t.Fatalf("cached no match = %+v, %v", env, ok)
	}
	svc.cache.putError(nsASIN.key("B0FAILED"), errors.New("down"))
	if _, ok := svc.CachedEnrichment(ctx, "B0FAILED", ""); ok {
		t.Fatal("a cached failure answered")
	}
	store.put(StoredEntry{Key: nsASIN.key("B0STORED"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"stored","recaps":[{"through":{"chapter":1},"text":"x"}]}}`), Expires: time.Now().Add(-time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0NOMATCH"), Version: storeVersion, Source: srv.URL, Expires: time.Now().Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0OTHER"), Version: storeVersion + 1, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"other"}}`), Expires: time.Now().Add(time.Hour)})
	if env, ok := svc.CachedEnrichment(ctx, "B0STORED", ""); !ok || env.Work.ID != "stored" || !env.HasRecaps() || env.HasCharacters() {
		t.Fatalf("stale stored row = %+v, %v", env, ok)
	}
	if env, ok := svc.CachedEnrichment(ctx, "B0NOMATCH", ""); !ok || env != nil {
		t.Fatalf("stored no match = %+v, %v", env, ok)
	}
	if _, ok := svc.CachedEnrichment(ctx, "B0OTHER", ""); ok {
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
