package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const datedSeries = `{
  "id":"mars","name":"Mars","authors":[{"id":"andy-weir","name":"Andy Weir"}],
  "works":[
    {"position":"1","work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"release_date":"2013-03-22","cover_url":"https://c/tm.jpg","added_at":"2024-01-02"}},
    {"position":"2","work":{"id":"artemis","title":"Artemis","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null}},
    {"position":"3","work":{"id":"next","title":"Next","authors":[],"series":null,"release_date":"2027-02","cover_url":null,"added_at":null}}
  ]
}`

// A rail entry carries its work's first release date (additive, omitted when
// unknown).
func TestRailWorksCarryReleaseDate(t *testing.T) {
	t.Parallel()
	m := fullMock()
	m.seriesJSON["mars"] = datedSeries
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	env, err := NewService(srv.URL, nil).Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatal(err)
	}
	works := env.Series[0].Works
	if works[0].ReleaseDate != "2013-03-22" || works[1].ReleaseDate != "" || works[2].ReleaseDate != "2027-02" {
		t.Fatalf("release dates = %q %q %q", works[0].ReleaseDate, works[1].ReleaseDate, works[2].ReleaseDate)
	}
}

func TestSeriesRecord(t *testing.T) {
	t.Parallel()
	m := fullMock()
	m.seriesJSON["mars"] = datedSeries
	m.seriesJSON["blank"] = `{"works":[]}`
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	rec, err := svc.Series(context.Background(), "mars")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "mars" || rec.Name != "Mars" || len(rec.Members) != 3 {
		t.Fatalf("record = %+v", rec)
	}
	first := rec.Members[0]
	if first.WorkID != "the-martian" || first.Position != "1" || first.ReleaseDate != "2013-03-22" ||
		first.AddedAt != "2024-01-02" || first.CoverURL != "https://c/tm.jpg" || first.Authors[0].Name != "Andy Weir" {
		t.Fatalf("first member = %+v", first)
	}
	if _, err := svc.Series(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown series = %v, want ErrNotFound", err)
	}
	if _, err := svc.Series(context.Background(), "blank"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 200 that is no series = %v, want ErrNotFound", err)
	}
	refs, err := svc.WorkSeries(context.Background(), "the-martian")
	if err != nil || len(refs) != 1 || refs[0] != (SeriesRef{ID: "mars", Name: "Mars"}) {
		t.Fatalf("WorkSeries = %+v, %v", refs, err)
	}
}

func TestWorkIDFromFeedItem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id, url     string
		work, state string
	}{
		{"tag:meta.audiosilo.app,2026:work/leviathan-falls/preorder", "", "leviathan-falls", "preorder"},
		{"tag:meta.audiosilo.app,2026:work/leviathan-falls/released", "https://x/works/other", "leviathan-falls", "released"},
		// A tag that isn't well formed falls back to the url.
		{"tag:meta.audiosilo.app,2026:work/leviathan-falls/someday", "https://meta.audiosilo.app/works/leviathan-falls", "leviathan-falls", ""},
		{"tag:meta.audiosilo.app,2026:work//released", "https://meta.audiosilo.app/works/a%20b", "", ""},
		{"tag:elsewhere,2026:work/x/released", "https://meta.audiosilo.app/works/x/", "x", ""},
		// Neither names a work.
		{"", "https://meta.audiosilo.app/series/the-expanse", "", ""},
		{"tag:meta.audiosilo.app,2026:work/a/b/released", "::not a url", "", ""},
		{"tag:meta.audiosilo.app,2026:work/" + strings.Repeat("x", 101) + "/released", "", "", ""},
		{"", "", "", ""},
	} {
		work, state := WorkIDFromFeedItem(tc.id, tc.url)
		if work != tc.work || state != tc.state {
			t.Errorf("WorkIDFromFeedItem(%q, %q) = %q, %q; want %q, %q", tc.id, tc.url, work, state, tc.work, tc.state)
		}
	}
}

func TestWatchFeed(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/watch/feed.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/feed+json; charset=utf-8")
		_, _ = w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[
		  {"id":"tag:meta.audiosilo.app,2026:work/a/preorder","url":"https://m/works/a","title":"S #2: A","date_published":"2026-11-12T00:00:00Z","tags":["preorder"]},
		  {"id":"junk","url":"https://m/series/s","title":"?","date_published":"2026-01-01T00:00:00Z"},
		  {"id":"","url":"https://m/works/b","title":"S #3: B","date_published":"2026-10-02T09:00:00Z","tags":["date unknown"]}
		]}`))
	}))
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	items, err := svc.WatchFeed(context.Background(), []string{"the-expanse", "mars"}, 90)
	if err != nil {
		t.Fatal(err)
	}
	if q := got.Load().(string); q != "s=the-expanse%2Cmars&window=90" {
		t.Fatalf("query = %q", q)
	}
	if len(items) != 2 || items[0] != (FeedItem{WorkID: "a", State: "preorder", Date: "2026-11-12", Title: "S #2: A"}) ||
		items[1].WorkID != "b" || items[1].Date != "2026-10-02" {
		t.Fatalf("items = %+v", items)
	}
	// Ids metaserve would refuse are never sent.
	if _, err := svc.WatchFeed(context.Background(), []string{"Not A Slug"}, 90); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("an invalid slug = %v", err)
	}
	many := make([]string, MaxFeedSeries+1)
	for i := range many {
		many[i] = "s"
	}
	if _, err := svc.WatchFeed(context.Background(), many, 90); err == nil {
		t.Fatal("more than MaxFeedSeries series must be refused")
	}
}

func TestIDKey(t *testing.T) {
	t.Parallel()
	if IDKey(" b00flijjsy ", "") != IDKey("B00FLIJJSY", "") || IDKey("", "") != "" {
		t.Fatal("IDKey must normalize, and be empty without identifiers")
	}
	if IDKey("B00FLIJJSY", "9780553418026") == IDKey("", "9780553418026") {
		t.Fatal("an asin is preferred over an isbn")
	}
}
