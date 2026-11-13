package meta

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

const (
	lvPage = "https://librivox.org/the-martian-by-andy-weir/"
	lvZip  = "https://archive.org/compress/martian_lv/formats=64KBPS%20MP3&file=/martian_lv.zip"
)

// wantLibrivox is the block the LibriVox fixtures below answer with.
var wantLibrivox = MetaLibrivox{ID: 47, URL: lvPage, ZipURL: lvZip}

// martianLibrivox is martianWork with the matched recording (rec2) a LibriVox
// one, as metaserve states it: the project id, its Internet Archive item and
// its links (rss and text too, which the server does not pass on).
var martianLibrivox = strings.Replace(martianWork, `{"id":"rec2",`,
	`{"id":"rec2","librivox_id":47,"archive_id":"martian_lv","links":{"librivox":"`+lvPage+`","zip":"`+lvZip+`","rss":"https://librivox.org/rss/47","text":"https://www.gutenberg.org/ebooks/1"},`, 1)

// TestLibrivoxPassThrough: a LibriVox recording's id and links reach the
// matched recording as its Librivox block (the wire's keys, and that nothing
// else of the LibriVox layer rides along, are api's TestMetaLibrivoxRecording).
func TestLibrivoxPassThrough(t *testing.T) {
	if martianLibrivox == martianWork {
		t.Fatal("fixture replace did not apply")
	}
	m := fullMock()
	m.workJSON = martianLibrivox
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	env, err := NewService(srv.URL, nil).Enrich(context.Background(), martianASIN, "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Recording == nil || env.Recording.Librivox == nil || *env.Recording.Librivox != wantLibrivox {
		t.Fatalf("recording = %+v, want librivox %+v", env.Recording, wantLibrivox)
	}
}

// TestLibrivoxAbsentOnRetail: a recording with no LibriVox project carries no
// librivox key at all (the field is omitted, not null).
func TestLibrivoxAbsentOnRetail(t *testing.T) {
	srv := httptest.NewServer(fullMock().handler())
	defer srv.Close()
	env, err := NewService(srv.URL, nil).Enrich(context.Background(), martianASIN, "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Recording == nil || env.Recording.Librivox != nil {
		t.Fatalf("recording = %+v, want no librivox", env.Recording)
	}
	body, _ := json.Marshal(env)
	if strings.Contains(string(body), `"librivox"`) {
		t.Fatalf("a retail recording's JSON names librivox: %s", body)
	}
}

// TestLibrivoxOf: the block exists only for a positive project id, and each
// link survives only as an absolute http(s) URL with a host; it is never built
// from the id.
func TestLibrivoxOf(t *testing.T) {
	for name, tc := range map[string]struct {
		rec  upstreamRecording
		want *MetaLibrivox
	}{
		"no id":           {upstreamRecording{Links: upstreamRecordingLinks{Librivox: lvPage}}, nil},
		"negative id":     {upstreamRecording{LibrivoxID: -1, Links: upstreamRecordingLinks{Librivox: lvPage}}, nil},
		"id, no links":    {upstreamRecording{LibrivoxID: 9}, &MetaLibrivox{ID: 9}},
		"both links":      {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: lvPage, Zip: lvZip}}, &MetaLibrivox{ID: 9, URL: lvPage, ZipURL: lvZip}},
		"plain http":      {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "http://librivox.org/x/"}}, &MetaLibrivox{ID: 9, URL: "http://librivox.org/x/"}},
		"javascript":      {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "javascript:alert(1)", Zip: lvZip}}, &MetaLibrivox{ID: 9, ZipURL: lvZip}},
		"relative":        {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "/the-martian/", Zip: "martian.zip"}}, &MetaLibrivox{ID: 9}},
		"scheme-relative": {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "//librivox.org/x/"}}, &MetaLibrivox{ID: 9}},
		"no host":         {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "https:///x/"}}, &MetaLibrivox{ID: 9}},
		"other schemes":   {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "ftp://librivox.org/x/", Zip: "data:text/html,hi"}}, &MetaLibrivox{ID: 9}},
		"unparseable":     {upstreamRecording{LibrivoxID: 9, Links: upstreamRecordingLinks{Librivox: "https://librivox.org/%zz"}}, &MetaLibrivox{ID: 9}},
	} {
		t.Run(name, func(t *testing.T) {
			got := librivoxOf(&tc.rec)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("librivoxOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLibrivoxSurvivesStore: the block is part of the envelope meta_cache
// persists, so a restarted server serves it from the store without asking
// upstream.
func TestLibrivoxSurvivesStore(t *testing.T) {
	e := newStoreEnv(t)
	e.m.workJSON = martianLibrivox
	if _, err := e.service().Enrich(context.Background(), martianASIN, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(time.Hour)
	env, err := e.service().Enrich(context.Background(), martianASIN, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.m.lookupHits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want the restart served from the store", got)
	}
	if lv := env.Recording.Librivox; lv == nil || *lv != wantLibrivox {
		t.Fatalf("persisted recording lost librivox: %+v", env.Recording)
	}
}

// TestMirrorLibrivox: in mirror mode the block comes from metaserve's own
// handler (pkg/query) over a real artifact. The querytest fixture holds no
// LibriVox recording, so the test states the LibriVox layer on its ASIN
// recording in the artifact's own columns before opening it.
func TestMirrorLibrivox(t *testing.T) {
	path := mirrortest.Artifact(t, "UPDATE recordings SET librivox_id=47, archive_id='martian_lv', link_librivox='"+lvPage+
		"', link_zip='"+lvZip+"', link_rss='https://librivox.org/rss/47' WHERE work_id='"+querytest.ASINWork+"' AND id='"+querytest.ASINRecording+"'")

	remote, remoteHits := mirrortest.Remote(t)
	s := NewService(remote.URL, nil)
	s.SetMirror(newFakeMirrorAt(t, path), nil)
	env, err := s.Enrich(context.Background(), querytest.ASIN, "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Recording == nil || env.Recording.ID != querytest.ASINRecording {
		t.Fatalf("recording = %+v", env.Recording)
	}
	if lv := env.Recording.Librivox; lv == nil || *lv != wantLibrivox {
		t.Fatalf("mirror recording librivox = %+v", lv)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times", n)
	}

	// The fixture's other recording states no LibriVox layer: no block.
	env, err = s.Enrich(context.Background(), "", querytest.ISBN)
	if err != nil || env.Recording == nil || env.Recording.Librivox != nil {
		t.Fatalf("ISBN recording = %+v, %v; want no librivox", env.Recording, err)
	}
}

// queryRecorder answers every request with body and records each request's
// path and query.
func queryRecorder(t *testing.T, body string) (*Service, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil), &got
}

// TestLibrivoxClientParams: a LibriVox lookup sends librivox_id alone (beside
// an asin or isbn metaserve would look those up instead), and the LibriVox
// search sends source=librivox, plus lang only when one is asked for; the
// console's match search sends neither.
func TestLibrivoxClientParams(t *testing.T) {
	t.Parallel()
	s, got := queryRecorder(t, `{"work":{"id":"w1"},"recording_id":"r1","results":[]}`)
	ctx := context.Background()
	if w, r, err := s.LibrivoxRecordingOf(ctx, 192); err != nil || w != "w1" || r != "r1" {
		t.Fatalf("LibrivoxRecordingOf = %q %q %v", w, r, err)
	}
	if _, err := s.LibrivoxSearch(ctx, "johnny crow", "", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LibrivoxSearch(ctx, "johnny crow", "de", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.searchWorks(ctx, "plain", 3, searchFilter{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/api/v1/lookup?librivox_id=192",
		"/api/v1/works/search?limit=20&q=johnny+crow&source=librivox",
		"/api/v1/works/search?lang=de&limit=5&q=johnny+crow&source=librivox",
		"/api/v1/works/search?limit=3&q=plain",
	}
	if strings.Join(*got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(*got, "\n"), strings.Join(want, "\n"))
	}
	// An id that is no project is never asked for.
	if _, _, err := s.LibrivoxRecordingOf(ctx, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("id 0 = %v, want ErrNotFound", err)
	}
	if len(*got) != len(want) {
		t.Fatalf("id 0 was sent upstream: %v", *got)
	}
}

// TestLibrivoxRecordingOfNeedsARecording: a lookup that names a work but no
// recording is not found, never "the work's first recording" (the identity a
// fetch records must be the project's own recording).
func TestLibrivoxRecordingOfNeedsARecording(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"no recording":    `{"work":{"id":"w1"},"recording_id":""}`,
		"blank recording": `{"work":{"id":"w1"},"recording_id":"  "}`,
		"no work":         `{"recording_id":"r1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := queryRecorder(t, body)
			if _, _, err := s.LibrivoxRecordingOf(context.Background(), 192); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestLibrivoxSearchHits: the hits are the cards' work id, title, authors,
// language and release date, in metaserve's order; a hit with no id is dropped.
func TestLibrivoxSearchHits(t *testing.T) {
	t.Parallel()
	s, _ := queryRecorder(t, `{"results":[`+
		`{"kind":"work","id":"johnny-crows-garden","title":"Johnny Crow's Garden","authors":[{"id":"l-leslie-brooke","name":"L. Leslie Brooke"}],"language":"en","release_date":"1903","cover_url":null,"narrators":[]},`+
		`{"kind":"work","id":"","title":"Broken"}]}`)
	hits, err := s.LibrivoxSearch(context.Background(), "crow", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []LibrivoxHit{{WorkID: "johnny-crows-garden", Title: "Johnny Crow's Garden",
		Authors: []MetaPersonRef{{ID: "l-leslie-brooke", Name: "L. Leslie Brooke"}}, Language: "en", ReleaseDate: "1903"}}
	if len(hits) != 1 || hits[0].WorkID != want[0].WorkID || hits[0].Title != want[0].Title ||
		len(hits[0].Authors) != 1 || hits[0].Authors[0] != want[0].Authors[0] || hits[0].Language != "en" || hits[0].ReleaseDate != "1903" {
		t.Fatalf("hits = %+v, want %+v", hits, want)
	}
	s, _ = queryRecorder(t, `{"results":[]}`)
	if hits, err := s.LibrivoxSearch(context.Background(), "nothing", "", 20); err != nil || hits == nil || len(hits) != 0 {
		t.Fatalf("no hits = %#v %v, want an empty list", hits, err)
	}
}

// TestLibrivoxWorkRecordings: a work lists only its LibriVox recordings (a
// store recording is left out), each with its project id, readers, length,
// sections, and its cover and page only as absolute http(s) URLs.
func TestLibrivoxWorkRecordings(t *testing.T) {
	t.Parallel()
	s, _ := queryRecorder(t, `{"id":"w1","title":"Johnny Crow's Garden","authors":[{"id":"a","name":"L. Leslie Brooke"}],"language":"en","recordings":[`+
		`{"id":"store","narrators":[{"id":"n","name":"Someone"}],"runtime_min":20,"asin":[{"region":"us","asin":"B000000001"}]},`+
		`{"id":"lv192","librivox_id":192,"narrators":[{"id":"r","name":"Reader"}],"runtime_min":12,"chapter_count":3,"cover_url":"https://archive.org/c.jpg","links":{"librivox":"https://librivox.org/johnny-crows-garden/"}},`+
		`{"id":"lv193","librivox_id":193,"cover_url":"javascript:alert(1)","links":{"librivox":"/relative/"}}]}`)
	w, err := s.LibrivoxWork(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Work.ID != "w1" || w.Work.Title != "Johnny Crow's Garden" || w.Work.Language != "en" || len(w.Work.Authors) != 1 {
		t.Fatalf("work = %+v", w.Work)
	}
	if len(w.Recordings) != 2 {
		t.Fatalf("recordings = %+v, want the two LibriVox ones", w.Recordings)
	}
	r := w.Recordings[0]
	if r.RecordingID != "lv192" || r.LibrivoxID != 192 || r.RuntimeMin != 12 || r.ChapterCount != 3 ||
		r.CoverURL != "https://archive.org/c.jpg" || r.URL != "https://librivox.org/johnny-crows-garden/" ||
		len(r.Narrators) != 1 || r.Narrators[0].Name != "Reader" {
		t.Fatalf("recording = %+v", r)
	}
	if r := w.Recordings[1]; r.CoverURL != "" || r.URL != "" || r.Narrators == nil {
		t.Fatalf("unsafe URLs passed through, or narrators not a list: %+v", r)
	}
	if _, err := s.LibrivoxWork(context.Background(), "  "); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blank id = %v, want ErrNotFound", err)
	}
}

// TestMirrorLibrivoxFetchReads: in mirror mode the LibriVox lookup and the
// source-filtered search are answered by metaserve's own handler over the local
// copy, never the remote service.
func TestMirrorLibrivoxFetchReads(t *testing.T) {
	path := mirrortest.Artifact(t,
		"UPDATE recordings SET librivox_id=192 WHERE work_id='"+querytest.ASINWork+"' AND id='"+querytest.ASINRecording+"'",
		"INSERT INTO recording_sources(work_id, recording_id, type) VALUES('"+querytest.ASINWork+"','"+querytest.ASINRecording+"','librivox')")
	remote, remoteHits := mirrortest.Remote(t)
	s := NewService(remote.URL, nil)
	s.SetMirror(newFakeMirrorAt(t, path), nil)
	ctx := context.Background()
	w, r, err := s.LibrivoxRecordingOf(ctx, 192)
	if err != nil || w != querytest.ASINWork || r != querytest.ASINRecording {
		t.Fatalf("mirror lookup = %q %q %v", w, r, err)
	}
	if _, _, err := s.LibrivoxRecordingOf(ctx, 193); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown project = %v, want ErrNotFound", err)
	}
	hits, err := s.LibrivoxSearch(ctx, "Hail Mary", "", 20)
	if err != nil || len(hits) != 1 || hits[0].WorkID != querytest.ASINWork {
		t.Fatalf("mirror search = %+v %v, want the LibriVox work alone", hits, err)
	}
	work, err := s.LibrivoxWork(ctx, querytest.ASINWork)
	if err != nil || len(work.Recordings) != 1 || work.Recordings[0].LibrivoxID != 192 {
		t.Fatalf("mirror work = %+v %v", work, err)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times", n)
	}
}
