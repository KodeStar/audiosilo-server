package meta

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/netguard"
)

// The default client never connects to the server's own network: a cover on
// loopback is refused before a byte is sent.
func TestFetchCoverRefusesPrivateAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()
	s := NewService("http://meta.invalid", nil)
	if _, err := s.FetchCover(context.Background(), srv.URL+"/cover.jpg", 1<<20); !errors.Is(err, netguard.ErrBlockedAddress) {
		t.Fatalf("loopback cover = %v, want netguard.ErrBlockedAddress", err)
	}
	if hit {
		t.Fatal("the request reached the loopback server")
	}
}

func TestFetchCover(t *testing.T) {
	img := bytes.Repeat([]byte{0xff}, 1000)
	mux := http.NewServeMux()
	mux.HandleFunc("/cover.jpg", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(img) })
	mux.HandleFunc("/moved.jpg", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cover.jpg", http.StatusFound)
	})
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.255.255.1/cover.jpg", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: the limit is found by reading.
		for range 3 {
			_, _ = w.Write(img)
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/declared", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(img)))
		_, _ = w.Write(img)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := NewService("http://meta.invalid", nil)
	// Loopback stands in for the public internet; the private range stays refused.
	s.covers = newCoverClient(func(a netip.Addr) bool { return a.IsLoopback() })
	ctx := context.Background()

	for _, path := range []string{"/cover.jpg", "/moved.jpg"} {
		if got, err := s.FetchCover(ctx, srv.URL+path, 1<<20); err != nil || !bytes.Equal(got, img) {
			t.Errorf("%s = %d bytes, %v; want the image", path, len(got), err)
		}
	}
	if _, err := s.FetchCover(ctx, srv.URL+"/to-private", 1<<20); !errors.Is(err, netguard.ErrBlockedAddress) {
		t.Errorf("redirect to a private address = %v, want netguard.ErrBlockedAddress", err)
	}
	if _, err := s.FetchCover(ctx, srv.URL+"/to-file", 1<<20); !errors.Is(err, ErrCoverURL) {
		t.Errorf("redirect to file: = %v, want ErrCoverURL", err)
	}
	if _, err := s.FetchCover(ctx, srv.URL+"/loop", 1<<20); err == nil {
		t.Error("a redirect loop was followed to the end")
	}
	var status *statusError
	if _, err := s.FetchCover(ctx, srv.URL+"/missing.jpg", 1<<20); !errors.As(err, &status) || status.code != 404 {
		t.Errorf("a 404 = %v, want its status", err)
	}
	if _, err := s.FetchCover(ctx, srv.URL+"/chunked", 2000); !errors.Is(err, media.ErrImageTooLarge) {
		t.Errorf("an undeclared oversize = %v, want ErrImageTooLarge", err)
	}
	if _, err := s.FetchCover(ctx, srv.URL+"/declared", 999); !errors.Is(err, media.ErrImageTooLarge) {
		t.Errorf("a declared oversize = %v, want ErrImageTooLarge", err)
	}
	if got, err := s.FetchCover(ctx, srv.URL+"/declared", 1000); err != nil || len(got) != 1000 {
		t.Errorf("exactly the limit = %d bytes, %v", len(got), err)
	}
	for _, bad := range []string{"", "cover.jpg", "/cover.jpg", "ftp://example.com/c.jpg", "file:///etc/passwd",
		"http://", "javascript:alert(1)", "https://example.com/" + string(bytes.Repeat([]byte("a"), maxCoverURLLen))} {
		if _, err := s.FetchCover(ctx, bad, 1<<20); !errors.Is(err, ErrCoverURL) {
			t.Errorf("FetchCover(%.40q) = %v, want ErrCoverURL", bad, err)
		}
	}
}

func TestHandsOutCover(t *testing.T) {
	e := &Enrichment{
		Recording: &MetaRecording{CoverURL: "https://img/rec.jpg"},
		Series: []MetaSeries{{
			Works: []MetaSeriesWork{{CoverURL: "https://img/1.jpg"}, {}},
			Orderings: []MetaSeriesOrdering{{
				Works: []MetaSeriesWork{{CoverURL: "https://img/chrono.jpg"}},
			}},
		}},
	}
	for _, u := range []string{"https://img/rec.jpg", "https://img/1.jpg", "https://img/chrono.jpg"} {
		if !e.HandsOutCover(u) {
			t.Errorf("HandsOutCover(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"", "https://img/2.jpg", "https://img/1.jpg?x", "https://IMG/1.jpg"} {
		if e.HandsOutCover(u) {
			t.Errorf("HandsOutCover(%q) = true, want false", u)
		}
	}
	var none *Enrichment
	if none.HandsOutCover("https://img/rec.jpg") || (&Enrichment{}).HandsOutCover("https://img/rec.jpg") {
		t.Error("an empty envelope hands out a cover")
	}
}
