package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

const (
	lvPage = "https://librivox.org/the-martian-by-andy-weir/"
	lvZip  = "https://archive.org/compress/martian_lv/formats=64KBPS%20MP3&file=/martian_lv.zip"
)

// martianLibrivox is martianWork with the matched recording (rec2) a LibriVox
// one, as metaserve states it: the project id, its Internet Archive item and
// its links (rss and text too, which the server does not pass on).
var martianLibrivox = strings.Replace(martianWork, `{"id":"rec2",`,
	`{"id":"rec2","librivox_id":47,"archive_id":"martian_lv","links":{"librivox":"`+lvPage+`","zip":"`+lvZip+`","rss":"https://librivox.org/rss/47","text":"https://www.gutenberg.org/ebooks/1"},`, 1)

// TestLibrivoxPassThrough: a LibriVox recording's id and links reach the
// matched recording as librivox {id, url, zip_url}, and nothing else of the
// LibriVox layer does.
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
	want := MetaLibrivox{ID: 47, URL: lvPage, ZipURL: lvZip}
	if env.Recording == nil || env.Recording.Librivox == nil || *env.Recording.Librivox != want {
		t.Fatalf("recording = %+v, want librivox %+v", env.Recording, want)
	}
	body, _ := json.Marshal(env.Recording)
	if !strings.Contains(string(body), `"librivox":{"id":47,"url":"`+lvPage+`","zip_url":"`) {
		t.Fatalf("recording JSON = %s", body)
	}
	for _, s := range []string{"archive_id", "rss", "gutenberg"} {
		if strings.Contains(string(body), s) {
			t.Fatalf("recording JSON carries %q: %s", s, body)
		}
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
		"no id":           {upstreamRecording{Links: &upstreamRecordingLinks{Librivox: lvPage}}, nil},
		"negative id":     {upstreamRecording{LibrivoxID: -1, Links: &upstreamRecordingLinks{Librivox: lvPage}}, nil},
		"id, no links":    {upstreamRecording{LibrivoxID: 9}, &MetaLibrivox{ID: 9}},
		"both links":      {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: lvPage, Zip: lvZip}}, &MetaLibrivox{ID: 9, URL: lvPage, ZipURL: lvZip}},
		"plain http":      {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "http://librivox.org/x/"}}, &MetaLibrivox{ID: 9, URL: "http://librivox.org/x/"}},
		"javascript":      {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "javascript:alert(1)", Zip: lvZip}}, &MetaLibrivox{ID: 9, ZipURL: lvZip}},
		"relative":        {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "/the-martian/", Zip: "martian.zip"}}, &MetaLibrivox{ID: 9}},
		"scheme-relative": {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "//librivox.org/x/"}}, &MetaLibrivox{ID: 9}},
		"no host":         {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "https:///x/"}}, &MetaLibrivox{ID: 9}},
		"other schemes":   {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "ftp://librivox.org/x/", Zip: "data:text/html,hi"}}, &MetaLibrivox{ID: 9}},
		"unparseable":     {upstreamRecording{LibrivoxID: 9, Links: &upstreamRecordingLinks{Librivox: "https://librivox.org/%zz"}}, &MetaLibrivox{ID: 9}},
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
	if lv := env.Recording.Librivox; lv == nil || *lv != (MetaLibrivox{ID: 47, URL: lvPage, ZipURL: lvZip}) {
		t.Fatalf("persisted recording lost librivox: %+v", env.Recording)
	}
}

// TestMirrorLibrivox: in mirror mode the block comes from metaserve's own
// handler (pkg/query) over a real artifact. The querytest fixture holds no
// LibriVox recording, so the test states the LibriVox layer on its ASIN
// recording in the artifact's own columns before opening it.
func TestMirrorLibrivox(t *testing.T) {
	path := querytest.Build(t, t.TempDir())
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := raw.Exec(`UPDATE recordings SET librivox_id=47, archive_id='martian_lv', link_librivox=?, link_zip=?, link_rss='https://librivox.org/rss/47' WHERE work_id=? AND id=?`,
		lvPage, lvZip, querytest.ASINWork, querytest.ASINRecording)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		_ = raw.Close()
		t.Fatalf("updated %d recordings, want 1", n)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := query.Open(path, "data-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := &fakeMirror{h: query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{})}
	m.ready.Store(true)

	remote, remoteHits := mirrortest.Remote(t)
	s := NewService(remote.URL, nil)
	s.SetMirror(m, nil)
	env, err := s.Enrich(context.Background(), querytest.ASIN, "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Recording == nil || env.Recording.ID != querytest.ASINRecording {
		t.Fatalf("recording = %+v", env.Recording)
	}
	if lv := env.Recording.Librivox; lv == nil || *lv != (MetaLibrivox{ID: 47, URL: lvPage, ZipURL: lvZip}) {
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
