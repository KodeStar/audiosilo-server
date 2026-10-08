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
