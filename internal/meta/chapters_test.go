package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecordingChapters(t *testing.T) {
	m := fullMock()
	mux := http.NewServeMux()
	mux.Handle("/", m.handler())
	var asked string
	mux.HandleFunc("GET /api/v1/works/{id}/recordings/{rid}/chapters", func(w http.ResponseWriter, r *http.Request) {
		asked = r.PathValue("id") + "/" + r.PathValue("rid")
		_, _ = w.Write([]byte(`{"chapters":[{"title":"Sol 1","start_ms":0,"length_ms":60000},{"title":"Sol 6","start_ms":60000,"length_ms":90000}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := NewService(srv.URL, nil)

	got, err := s.RecordingChapters(context.Background(), "b00b5hi6pm", "")
	if err != nil {
		t.Fatal(err)
	}
	if asked != "the-martian/rec2" {
		t.Errorf("asked for %q, want the lookup's work and recording", asked)
	}
	want := []Chapter{{Title: "Sol 1", LengthMS: 60000}, {Title: "Sol 6", StartMS: 60000, LengthMS: 90000}}
	if got.WorkID != "the-martian" || got.RecordingID != "rec2" || len(got.Chapters) != 2 || got.Chapters[0] != want[0] || got.Chapters[1] != want[1] {
		t.Errorf("got %+v", got)
	}
}

// TestMatchChapters: a recorded match's chapters are its recording's, read off
// the work with no lookup; one naming no recording, or one the work doesn't
// list, has none (server#375), and nothing is asked for. A work the community
// doesn't have (or a blank id) is ErrNoWork, which a caller tells from those.
func TestMatchChapters(t *testing.T) {
	m := fullMock()
	mux := http.NewServeMux()
	mux.Handle("/", m.handler())
	var asked []string
	mux.HandleFunc("GET /api/v1/works/{id}/recordings/{rid}/chapters", func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.PathValue("id")+"/"+r.PathValue("rid"))
		_, _ = w.Write([]byte(`{"chapters":[{"title":"Sol 1","start_ms":0,"length_ms":60000}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := NewService(srv.URL, nil)
	ctx := context.Background()

	got, err := s.MatchChapters(ctx, "the-martian", "rec1")
	if err != nil || got.WorkID != "the-martian" || got.RecordingID != "rec1" || len(got.Chapters) != 1 {
		t.Fatalf("listed recording = %+v, %v", got, err)
	}
	for _, c := range [][2]string{{"the-martian", "rec-of-another-work"}, {"the-martian", ""}} {
		if got, err := s.MatchChapters(ctx, c[0], c[1]); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoWork) {
			t.Errorf("MatchChapters(%q, %q) = %+v, %v; want ErrNotFound, the work being there", c[0], c[1], got, err)
		}
	}
	if got, err := s.MatchChapters(ctx, " ", "rec1"); !errors.Is(err, ErrNoWork) || !errors.Is(err, ErrNotFound) {
		t.Errorf("a blank work = %+v, %v; want ErrNoWork", got, err)
	}
	if len(asked) != 1 || asked[0] != "the-martian/rec1" || m.lookupHits.Load() != 0 {
		t.Errorf("asked %v with %d lookups, want only the listed recording's and none", asked, m.lookupHits.Load())
	}

	// The community no longer has the work: ErrNoWork, for a match naming a
	// recording and for one naming none, while an outage is neither.
	m.workCode = http.StatusNotFound
	for _, rec := range []string{"rec1", ""} {
		if got, err := s.MatchChapters(ctx, "the-martian", rec); !errors.Is(err, ErrNoWork) {
			t.Errorf("a work the community lacks, recording %q = %+v, %v; want ErrNoWork", rec, got, err)
		}
	}
	m.workCode = http.StatusInternalServerError
	if got, err := s.MatchChapters(ctx, "the-martian", "rec1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("an outage = %+v, %v; want the service's failure", got, err)
	}
}

func TestRecordingChaptersNeedsTheRecording(t *testing.T) {
	m := fullMock()
	m.lookupJSON = `{"work":{"id":"the-martian","title":"The Martian"},"recording_id":""}`
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	// A match without a recording is no match: the work's first recording may
	// be another edition.
	if _, err := NewService(srv.URL, nil).RecordingChapters(context.Background(), "B00B5HI6PM", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err %v, want ErrNotFound", err)
	}
	if _, err := NewService(srv.URL, nil).RecordingChapters(context.Background(), "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("no identifier: err %v, want ErrNotFound", err)
	}
}

func TestRecordingChaptersOlderMetaserve(t *testing.T) {
	// No chapters route: the mock's works/{id} answers the long path with 404.
	srv := httptest.NewServer(fullMock().handler())
	defer srv.Close()
	got, err := NewService(srv.URL, nil).RecordingChapters(context.Background(), "B00B5HI6PM", "")
	if err != nil || got == nil || len(got.Chapters) != 0 {
		t.Errorf("got %+v, %v; want no chapters and no error", got, err)
	}
}

func TestRecordingChaptersUpstreamDown(t *testing.T) {
	m := fullMock()
	m.lookupCode = http.StatusBadGateway
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	_, err := NewService(srv.URL, nil).RecordingChapters(context.Background(), "B00B5HI6PM", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err %v, want an upstream error", err)
	}
}
