package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

// TestCandidatesByWorkID: a query naming a work (a refresh of a book matched with
// no ASIN or ISBN) asks works/{id} alone, never works/match: a retired id's
// redirect is followed, so the candidate is the record it was merged into, with
// the series rules applied; the recording the book was matched to is kept while
// the work lists it, else none (DefaultRecording picks by length). An id the
// community doesn't know is no candidate, not an outage; a 5xx is one.
func TestCandidatesByWorkID(t *testing.T) {
	t.Parallel()
	var matchAsked atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, _ *http.Request) {
		matchAsked.Add(1)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "old-slug":
			http.Redirect(w, r, "/api/v1/works/new-slug", http.StatusMovedPermanently)
		case "new-slug":
			_, _ = w.Write([]byte(`{"id":"new-slug","title":"T","authors":[{"id":"a","name":"A"}],
				"series":[{"id":"merged","name":"Merged Series","position":"3"}],
				"recordings":[{"id":"short","runtime_min":100},{"id":"long","runtime_min":600}]}`))
		case "down":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	ctx := context.Background()

	cands, err := s.Candidates(ctx, MatchQuery{WorkID: "old-slug", RecordingID: "short", Duration: 600 * 60, Title: "ignored"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %+v %v", cands, err)
	}
	c := cands[0]
	if c.WorkID != "new-slug" || c.Score != 100 || c.Reasons != nil || len(c.Series) != 1 || c.Series[0].Name != "Merged Series" {
		t.Fatalf("candidate = %+v, want the merged record", c)
	}
	if c.RecordingID != "short" || c.DefaultRecordingID != "short" {
		t.Fatalf("recording = %q default %q, want the recorded one kept", c.RecordingID, c.DefaultRecordingID)
	}
	// A recording the work no longer lists: none named, the length decides.
	cands, err = s.Candidates(ctx, MatchQuery{WorkID: "new-slug", RecordingID: "gone", Duration: 600 * 60})
	if err != nil || len(cands) != 1 || cands[0].RecordingID != "" || cands[0].DefaultRecordingID != "long" {
		t.Fatalf("a dropped recording = %+v %v", cands, err)
	}
	if n := matchAsked.Load(); n != 0 {
		t.Fatalf("works/match asked %d times, want never", n)
	}
	if cands, err := s.Candidates(ctx, MatchQuery{WorkID: "unknown"}); err != nil || len(cands) != 0 {
		t.Fatalf("an unknown work = %+v %v, want none and no error", cands, err)
	}
	if _, err := s.Candidates(ctx, MatchQuery{WorkID: "down"}); err == nil {
		t.Fatal("a 5xx was no error")
	}
}

// TestCandidatesByWorkIDInMirrorMode: the local copy answers a work query, its
// retired slug's redirect followed as online, and an id it lacks is none; the
// remote service is never asked.
func TestCandidatesByWorkIDInMirrorMode(t *testing.T) {
	t.Parallel()
	remote, remoteHits := mirrortest.Remote(t)
	s := NewService(remote.URL, nil)
	s.SetMirror(newFakeMirror(t), nil)
	ctx := context.Background()
	cands, err := s.Candidates(ctx, MatchQuery{WorkID: querytest.RetiredWork, RecordingID: querytest.ASINRecording})
	if err != nil || len(cands) != 1 || cands[0].WorkID != querytest.RetiredWorkTarget || cands[0].RecordingID != querytest.ASINRecording {
		t.Fatalf("a retired slug in mirror mode = %+v %v, want %s", cands, err, querytest.RetiredWorkTarget)
	}
	if cands, err := s.Candidates(ctx, MatchQuery{WorkID: "no-such-work"}); err != nil || len(cands) != 0 {
		t.Fatalf("an unknown work in mirror mode = %+v %v", cands, err)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times", n)
	}
}
