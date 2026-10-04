package meta

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPing(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	s := NewService(srv.URL+"/", nil)
	h := s.Ping()
	if !h.Reachable || h.Error != "" || h.CheckedAt.IsZero() {
		t.Fatalf("Ping = %+v, want reachable", h)
	}
	s.Ping()
	if hits.Load() != 1 {
		t.Fatalf("a second Ping within a minute must reuse the answer, got %d requests", hits.Load())
	}

	status.Store(http.StatusServiceUnavailable)
	s2 := NewService(srv.URL, nil)
	if h := s2.Ping(); h.Reachable || h.Error == "" {
		t.Fatalf("an unhealthy service = %+v", h)
	}
	s3 := NewService("http://127.0.0.1:1", nil)
	if h := s3.Ping(); h.Reachable || h.Error != "not responding" {
		t.Fatalf("no service = %+v", h)
	}
}
