//go:build podcastdiscoverytest

package discovery

import (
	"context"
	"testing"
	"time"
)

// A podcastdiscoverytest build sends every directory request to the origin
// variable's address, and only that address gets the loopback exception.
func TestTaggedOriginHonoured(t *testing.T) {
	if !testOriginBuild {
		t.Fatal("the tag is set")
	}
	f := newFake(t)
	t.Setenv(TestOriginEnv, "http://"+f.origin().String())
	s := New(Options{Now: (&clock{t: time.Unix(1, 0)}).Now, IndexKey: "k", IndexSecret: "s"})
	if s.ends.itunes != f.srv.URL+"/itunes/search" || s.ends.index != f.srv.URL+"/index/api/1.0/search/byterm" {
		t.Fatalf("endpoints %+v", s.ends)
	}
	if _, e := s.Search(context.Background(), "1", query(t, "q=fixture")); e != nil || f.count("/itunes/search") != 1 {
		t.Fatalf("= %v", e)
	}
	// A malformed origin is ignored: the fixed origins again.
	t.Setenv(TestOriginEnv, "http://127.0.0.1:"+"1/x")
	if s := New(Options{}); s.ends.itunes != itunesSearchURL {
		t.Fatalf("malformed honoured %+v", s.ends)
	}
	// Another loopback port stays refused.
	t.Setenv(TestOriginEnv, "http://127.0.0.1:1")
	s = New(Options{Now: time.Now})
	s.ends.itunes = f.srv.URL + "/itunes/search"
	if _, e := s.Search(context.Background(), "1", query(t, "q=fixture")); e == nil || f.count("/itunes/search") != 1 {
		t.Fatalf("other port = %v", e)
	}
}
