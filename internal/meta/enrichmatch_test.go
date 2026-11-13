package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

// TestEnrichMatch: a recorded match composes the envelope from works/{id}
// alone (no lookup), as the recording it names, rails included; a match naming
// no recording takes the first, as a lookup without one does.
func TestEnrichMatch(t *testing.T) {
	m := fullMock()
	m.workJSON = martianLibrivox
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env, err := svc.EnrichMatch(context.Background(), "the-martian", "rec2")
	if err != nil {
		t.Fatal(err)
	}
	if !env.Matched || env.Work.ID != "the-martian" || env.WebURL != srv.URL+"/work?id=the-martian" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Recording == nil || env.Recording.ID != "rec2" || env.Recording.Librivox == nil || *env.Recording.Librivox != wantLibrivox {
		t.Fatalf("recording = %+v, want rec2 with its librivox block", env.Recording)
	}
	if len(env.Series) != 1 || env.Series[0].ID != "mars" {
		t.Fatalf("series = %+v", env.Series)
	}
	if n := m.lookupHits.Load(); n != 0 {
		t.Fatalf("a recorded match asked the lookup %d times", n)
	}

	env, err = svc.EnrichMatch(context.Background(), "the-martian", "rec1")
	if err != nil || env.Recording.ID != "rec1" || env.Recording.Librivox != nil {
		t.Fatalf("rec1 = %+v, %v", env.Recording, err)
	}
	env, err = svc.EnrichMatch(context.Background(), "the-martian", "")
	if err != nil || env.Recording.ID != "rec1" {
		t.Fatalf("no recording named = %+v, %v; want the first", env.Recording, err)
	}

	if _, err := svc.EnrichMatch(context.Background(), " ", "rec1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a blank work id = %v, want ErrNotFound", err)
	}
}

// TestEnrichMatchTakesNoWorkSlot: a recorded match's work id comes from a book
// the server holds, as Enrich's, so its read never queues behind the
// caller-chosen work fetches (workSem): with every slot taken it still answers,
// where a queued read would run out its compose budget and cache the wait as an
// upstream failure.
func TestEnrichMatchTakesNoWorkSlot(t *testing.T) {
	srv := httptest.NewServer(fullMock().handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	svc.composeBudget = 2 * time.Second
	for range cap(svc.workSem) {
		svc.workSem <- struct{}{}
	}
	if env, err := svc.EnrichMatch(context.Background(), "the-martian", "rec1"); err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("with every work slot taken = %+v, %v; want the match", env, err)
	}
}

// TestEnrichMatchCached: a second ask is the cache's, in a key space of its
// own (the same work by ASIN is its own entry), and an unknown work is a cached
// "no match".
func TestEnrichMatchCached(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	ctx := context.Background()

	for range 2 {
		if _, err := svc.EnrichMatch(ctx, "the-martian", "rec2"); err != nil {
			t.Fatal(err)
		}
	}
	if n := m.workHits.Load(); n != 1 {
		t.Fatalf("work hits = %d, want 1 (the second ask cached)", n)
	}
	if _, err := svc.Enrich(ctx, martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	if n := m.workHits.Load(); n != 2 {
		t.Fatalf("work hits = %d, want the ASIN's own compose", n)
	}

	if got, want := matchKey("the-martian", "rec2"), "m:the-martian/rec2"; got != want {
		t.Fatalf("matchKey = %q, want %q", got, want)
	}
	if matchKey("a/b", "c") == matchKey("a", "b/c") {
		t.Fatal("matchKey must keep the two ids apart")
	}

	m.workCode = http.StatusNotFound
	for range 2 {
		if _, err := svc.EnrichMatch(ctx, "gone", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unknown work = %v, want ErrNotFound", err)
		}
	}
	if n := m.workHits.Load(); n != 3 {
		t.Fatalf("work hits = %d, want the miss cached", n)
	}
}

// TestEnrichMatchStore: the answer persists like an identifier's, so a restart
// serves it without upstream, and a stored answer outlives an outage.
func TestEnrichMatchStore(t *testing.T) {
	e := newStoreEnv(t)
	e.m.workJSON = martianLibrivox
	if _, err := e.service().EnrichMatch(context.Background(), "the-martian", "rec2"); err != nil {
		t.Fatal(err)
	}
	if row, ok := e.store.row(matchKey("the-martian", "rec2")); !ok || len(row.Payload) == 0 {
		t.Fatalf("persisted row = %+v, %v", row, ok)
	}

	e.clk.advance(time.Hour)
	env, err := e.service().EnrichMatch(context.Background(), "the-martian", "rec2")
	if err != nil || env.Recording.Librivox == nil || *env.Recording.Librivox != wantLibrivox {
		t.Fatalf("restart = %+v, %v", env, err)
	}
	if n := e.m.workHits.Load(); n != 1 {
		t.Fatalf("work hits = %d, want the restart served from the store", n)
	}

	// A day on the row is stale; with the upstream down it is still served.
	e.clk.advance(48 * time.Hour)
	e.down.Store(true)
	env, err = e.service().EnrichMatch(context.Background(), "the-martian", "rec2")
	if err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("outage = %+v, %v; want the stored answer", env, err)
	}
}

// TestMirrorEnrichMatch: in mirror mode a recorded match is answered by the
// local copy (pkg/query's works/{id}), LibriVox block included, and the remote
// service is never asked.
func TestMirrorEnrichMatch(t *testing.T) {
	path := mirrortest.Artifact(t, "UPDATE recordings SET librivox_id=47, link_librivox='"+lvPage+
		"', link_zip='"+lvZip+"' WHERE work_id='"+querytest.ASINWork+"' AND id='"+querytest.ASINRecording+"'")
	remote, remoteHits := mirrortest.Remote(t)
	s := NewService(remote.URL, nil)
	s.SetMirror(newFakeMirrorAt(t, path), nil)

	env, err := s.EnrichMatch(context.Background(), querytest.ASINWork, querytest.ASINRecording)
	if err != nil {
		t.Fatal(err)
	}
	if env.Work.ID != querytest.ASINWork || env.Recording == nil || env.Recording.ID != querytest.ASINRecording {
		t.Fatalf("envelope = work %+v recording %+v", env.Work, env.Recording)
	}
	if lv := env.Recording.Librivox; lv == nil || *lv != wantLibrivox {
		t.Fatalf("mirror librivox = %+v", lv)
	}
	if _, err := s.EnrichMatch(context.Background(), "no-such-work", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown work in the copy = %v, want ErrNotFound", err)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times", n)
	}
}
