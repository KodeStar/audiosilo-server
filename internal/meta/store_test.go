package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is an in-memory Store standing in for the catalog's meta_cache table,
// with counters so a test can tell a store read from an upstream one.
type memStore struct {
	mu       sync.Mutex
	rows     map[string]StoredEntry
	loads    atomic.Int32
	failLoad atomic.Bool
	failSave atomic.Bool
}

func newMemStore() *memStore { return &memStore{rows: map[string]StoredEntry{}} }

func (m *memStore) Load(_ context.Context, key string) (StoredEntry, bool, error) {
	m.loads.Add(1)
	if m.failLoad.Load() {
		return StoredEntry{}, false, errors.New("disk on fire")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[key]
	return e, ok, nil
}

func (m *memStore) Save(_ context.Context, e StoredEntry) error {
	if m.failSave.Load() {
		return errors.New("disk full")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[e.Key] = e
	return nil
}

func (m *memStore) row(key string) (StoredEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[key]
	return e, ok
}

func (m *memStore) put(e StoredEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[e.Key] = e
}

// storeEnv is a full metaserve mock that can be taken down as a whole (every
// route 500s while down is set), plus a clock and a shared store, so a test can
// stand up a "restarted" Service over the same store and upstream. Upstream
// knows one book (the Martian): the asin B0NOPE and any other work id 404.
type storeEnv struct {
	m    *mockMeta
	down atomic.Bool
	// lookups counts every lookup request, including the ones this wrapper
	// answers itself (an outage, B0NOPE) before the mock sees them.
	lookups atomic.Int32
	srv     *httptest.Server
	clk     *clock
	store   *memStore
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	e := &storeEnv{m: fullMock(), clk: &clock{t: time.Unix(1_700_000_000, 0)}, store: newMemStore()}
	h := e.m.handler()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/lookup" {
			e.lookups.Add(1)
		}
		switch {
		case e.down.Load():
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/api/v1/lookup" && r.URL.Query().Get("asin") == "B0NOPE",
			strings.HasPrefix(r.URL.Path, "/api/v1/works/") && r.URL.Path != "/api/v1/works/the-martian":
			w.WriteHeader(http.StatusNotFound)
		default:
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// service is a fresh Service (an empty memory cache: a restart) over the shared
// store, upstream and clock.
func (e *storeEnv) service() *Service {
	svc := NewService(e.srv.URL, e.clk.now)
	svc.SetStore(e.store)
	return svc
}

const martianASIN = "B00FLIJJSY"

// TestStoreReadThroughAfterRestart: a positive enrichment is persisted, a
// restarted Service serves it from the store without an upstream call, and the
// row warms memory for its REMAINING TTL (not a fresh day), after which the
// upstream is asked again.
func TestStoreReadThroughAfterRestart(t *testing.T) {
	e := newStoreEnv(t)
	if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	row, ok := e.store.row(nsASIN.key(martianASIN))
	if !ok || row.Version != storeVersion || row.Source != e.srv.URL || len(row.Payload) == 0 {
		t.Fatalf("persisted row = %+v, %v", row, ok)
	}
	if want := e.clk.now().Add(positiveTTL); !row.Expires.Equal(want) {
		t.Fatalf("row expires %v, want %v", row.Expires, want)
	}

	// Restart 23h later: served from the store, no upstream call.
	e.clk.advance(23 * time.Hour)
	svc := e.service()
	env, err := svc.Enrich(context.Background(), martianASIN, "")
	if err != nil || env.Work == nil || env.Work.ID != "the-martian" || len(env.Series) != 1 {
		t.Fatalf("warm Enrich = %+v, %v", env, err)
	}
	if env.Work.Attribution == nil || env.Recording == nil || env.Recording.ChapterCount != 12 {
		t.Fatalf("persisted envelope lost fields: %+v / %+v", env.Work, env.Recording)
	}
	if got := e.m.lookupHits.Load(); got != 1 {
		t.Fatalf("lookups after warm restart = %d, want 1", got)
	}
	// Memory is warm now: the next call reads neither the store nor upstream.
	loads := e.store.loads.Load()
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	if e.store.loads.Load() != loads || e.m.lookupHits.Load() != 1 {
		t.Fatalf("second call reached the store (%d loads) or upstream (%d lookups)", e.store.loads.Load()-loads, e.m.lookupHits.Load())
	}

	// Past the row's own expiry (one hour on, not 24): memory has let go, the
	// row is stale, and the upstream answers afresh.
	e.clk.advance(time.Hour + time.Second)
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.m.lookupHits.Load(); got != 2 {
		t.Fatalf("lookups past the remaining TTL = %d, want 2", got)
	}
}

// TestStoreStaleOnOutage: with the upstream down, a positive row however stale
// is served (not a 502), held in memory for errorTTL only so the upstream is
// retried soon, and never overwritten by the failure.
func TestStoreStaleOnOutage(t *testing.T) {
	e := newStoreEnv(t)
	if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.row(nsASIN.key(martianASIN))

	e.clk.advance(30 * 24 * time.Hour)
	e.down.Store(true)
	svc := e.service()
	env, err := svc.Enrich(context.Background(), martianASIN, "")
	if err != nil || env == nil || env.Work.ID != "the-martian" {
		t.Fatalf("outage Enrich = %+v, %v; want the stale envelope", env, err)
	}
	after, _ := e.store.row(nsASIN.key(martianASIN))
	if !after.Expires.Equal(before.Expires) || string(after.Payload) != string(before.Payload) {
		t.Fatalf("the failure rewrote the row: %+v", after)
	}

	// Held for errorTTL: a call within it does not reach the store again.
	loads := e.store.loads.Load()
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	if e.store.loads.Load() != loads {
		t.Fatal("the stale answer was not held in memory")
	}
	// Past errorTTL the upstream is asked again: back up now, so the fresh
	// answer replaces the row.
	e.down.Store(false)
	e.clk.advance(errorTTL + time.Second)
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.m.lookupHits.Load(); got != 2 {
		t.Fatalf("lookups after recovery = %d, want 2", got)
	}
	if row, _ := e.store.row(nsASIN.key(martianASIN)); !row.Expires.After(before.Expires) {
		t.Fatalf("recovery did not refresh the row: %+v", row)
	}

	// A caller that went away gets its own error, not the stale fallback: the
	// failure is the caller's, not the upstream's.
	e.clk.advance(positiveTTL + time.Second)
	e.down.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.service().Enrich(ctx, martianASIN, ""); err == nil {
		t.Fatal("a cancelled caller was served the stale fallback")
	}
}

// TestStoreNotFoundPersisted: a "no match" is persisted for notFoundTTL and
// answered from the store after a restart; a stale one is simply asked again.
func TestStoreNotFoundPersisted(t *testing.T) {
	e := newStoreEnv(t)
	if _, err := e.service().Enrich(context.Background(), "B0NOPE", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	row, ok := e.store.row(nsASIN.key("B0NOPE"))
	if !ok || len(row.Payload) != 0 || !row.Expires.Equal(e.clk.now().Add(notFoundTTL)) {
		t.Fatalf("not-found row = %+v, %v", row, ok)
	}
	if _, err := e.service().Enrich(context.Background(), "B0NOPE", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restart err = %v, want ErrNotFound", err)
	}
	if got := e.lookups.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (the restart read the store)", got)
	}
	// A stale "no match" is no fallback: past its TTL, with the upstream down,
	// the caller gets the outage.
	e.clk.advance(notFoundTTL + time.Second)
	e.down.Store(true)
	if _, err := e.service().Enrich(context.Background(), "B0NOPE", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("stale not-found during an outage: err = %v, want the transport error", err)
	}
}

// TestStoreTransportErrorNotPersisted: an outage is cached in memory only.
func TestStoreTransportErrorNotPersisted(t *testing.T) {
	e := newStoreEnv(t)
	e.down.Store(true)
	if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err == nil {
		t.Fatal("expected the outage")
	}
	if _, ok := e.store.row(nsASIN.key(martianASIN)); ok {
		t.Fatal("a transport error was persisted")
	}
}

// TestStoreIncompletePersistedBriefly: an envelope missing a failed rail is
// persisted for errorTTL, exactly as memory holds it, so a restart retries it
// as soon.
func TestStoreIncompletePersistedBriefly(t *testing.T) {
	e := newStoreEnv(t)
	e.m.seriesFailing = map[string]bool{"mars": true}
	e.m.failSeries.Store(true)
	if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	row, ok := e.store.row(nsASIN.key(martianASIN))
	if !ok || !row.Expires.Equal(e.clk.now().Add(errorTTL)) {
		t.Fatalf("incomplete row = %+v, %v; want expiry now+errorTTL", row, ok)
	}
}

// TestStoreWorks: a positive work is persisted and served after a restart, and
// outlives an outage; a work-id miss is never persisted (the id is the caller's
// choice).
func TestStoreWorks(t *testing.T) {
	e := newStoreEnv(t)
	svc := e.service()
	if _, err := svc.Work(context.Background(), "the-martian"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.store.row(nsWork.key("the-martian")); !ok {
		t.Fatal("positive work not persisted")
	}
	if _, err := svc.Work(context.Background(), "no-such-work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.store.row(nsWork.key("no-such-work")); ok {
		t.Fatal("a work-id miss was persisted")
	}

	hits := e.m.workHits.Load()
	w, err := e.service().Work(context.Background(), "the-martian")
	if err != nil || w.ID != "the-martian" || w.Attribution == nil {
		t.Fatalf("warm Work = %+v, %v", w, err)
	}
	if e.m.workHits.Load() != hits {
		t.Fatal("the restarted service fetched a persisted work upstream")
	}

	e.clk.advance(7 * 24 * time.Hour)
	e.down.Store(true)
	if w, err := e.service().Work(context.Background(), "the-martian"); err != nil || w.ID != "the-martian" {
		t.Fatalf("outage Work = %+v, %v; want the stale work", w, err)
	}
}

// TestStoreIgnoresOtherVersionsAndSources: a row of another format version, or
// from another metaserve, is never served, however fresh.
func TestStoreIgnoresOtherVersionsAndSources(t *testing.T) {
	for name, mutate := range map[string]func(*StoredEntry){
		"version": func(r *StoredEntry) { r.Version = storeVersion + 1 },
		"source":  func(r *StoredEntry) { r.Source = "https://elsewhere.example" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newStoreEnv(t)
			row := StoredEntry{
				Key: nsASIN.key(martianASIN), Version: storeVersion, Source: e.srv.URL,
				Payload: []byte(`{"matched":true,"work":{"id":"impostor","title":"X","authors":[],"language":"en"},"web_url":"x"}`),
				Expires: e.clk.now().Add(time.Hour),
			}
			mutate(&row)
			e.store.put(row)
			env, err := e.service().Enrich(context.Background(), martianASIN, "")
			if err != nil || env.Work.ID != "the-martian" {
				t.Fatalf("Enrich = %+v, %v; want the upstream answer", env, err)
			}
			if got := e.m.lookupHits.Load(); got != 1 {
				t.Fatalf("lookups = %d, want 1", got)
			}
			// ...nor used as the outage fallback.
			e.store.put(row)
			e.down.Store(true)
			if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err == nil {
				t.Fatal("an ignored row was served as the outage fallback")
			}
		})
	}
}

// TestStoreFailuresDegradeToMemory: a store that fails to read or write never
// fails a lookup.
func TestStoreFailuresDegradeToMemory(t *testing.T) {
	e := newStoreEnv(t)
	e.store.failLoad.Store(true)
	e.store.failSave.Store(true)
	svc := e.service()
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatalf("Enrich with a broken store = %v", err)
	}
	if _, err := svc.Work(context.Background(), "the-martian"); err != nil {
		t.Fatalf("Work with a broken store = %v", err)
	}
	// Memory still caches.
	if _, err := svc.Enrich(context.Background(), martianASIN, ""); err != nil || e.m.lookupHits.Load() != 1 {
		t.Fatalf("memory cache bypassed: %v, %d lookups", err, e.m.lookupHits.Load())
	}
}
