// Package ghfake is a fake GitHub Releases API publishing audiosilo-meta data
// releases, for tests only (the metadata mirror's own, the api's and the
// launcher's): the release list with ETag/304, data releases interleaved with
// code releases (v*, no data assets) as the real repository has them, and the
// assets (meta.sqlite.gz built from querytest's real artifact, and its digest).
package ghfake

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release"

	_ "modernc.org/sqlite" // the artifact's schema_version rewrite
)

// Artifact is one data release's assets: the gzipped artifact and its digest
// file.
type Artifact struct {
	GZ     []byte
	Digest string // the sha256sum line, as audiosilo-meta's release.yml writes it
}

// Fixture gzips querytest's artifact. schema, when non-zero, first rewrites the
// copy's meta(schema_version): an artifact newer than the code reading it.
func Fixture(tb testing.TB, schema int) Artifact {
	tb.Helper()
	path := querytest.Build(tb, tb.TempDir())
	if schema != 0 {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `UPDATE meta SET value=? WHERE key='schema_version'`, schema); err != nil {
			tb.Fatal(err)
		}
		if err := db.Close(); err != nil {
			tb.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return Gzip(tb, raw)
}

// Gzip makes an artifact of any bytes, with a matching digest.
func Gzip(tb testing.TB, raw []byte) Artifact {
	tb.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		tb.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return Artifact{GZ: buf.Bytes(), Digest: hex.EncodeToString(sum[:]) + "  " + release.DataAsset + "\n"}
}

// Release is one release on the fake: a data release when Art is set, a code
// release otherwise.
type Release struct {
	Tag       string
	Published time.Time
	Art       *Artifact
}

// Releases is a realistic list: the data releases tags (published an hour
// apart, in order) between two code releases, the newest data release listed
// after older ones and a code release published later than all of them - so
// neither "the first listed" nor "GitHub's latest" is the answer.
func Releases(art Artifact, tags ...string) []Release {
	t0 := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	out := []Release{{Tag: "v0.21.0", Published: t0.Add(time.Duration(len(tags)+5) * time.Hour)}}
	for i, tag := range tags {
		out = append(out, Release{Tag: tag, Published: t0.Add(time.Duration(i) * time.Hour), Art: &art})
	}
	return append(out, Release{Tag: "v0.20.0", Published: t0.Add(-time.Hour)})
}

// Server is the fake. Its counts say what was asked.
type Server struct {
	URL string
	srv *httptest.Server

	mu        sync.Mutex
	rels      []Release
	etag      string
	lists     int
	notMod    int
	downloads int
	agents    []string
	hold      chan struct{}
}

// New starts a fake publishing rels (closed with the test).
func New(tb testing.TB, rels ...Release) *Server {
	tb.Helper()
	s := &Server{}
	s.Publish(rels...)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+release.DefaultRepo+"/releases", s.list)
	mux.HandleFunc("GET /dl/{tag}/{name}", s.asset)
	s.srv = httptest.NewServer(mux)
	s.URL = s.srv.URL
	tb.Cleanup(s.srv.Close)
	return s
}

// Client is a release client pointed at the fake.
func (s *Server) Client(opts ...release.Option) *release.Client {
	return release.New("", "", append([]release.Option{release.WithAPIBase(s.URL)}, opts...)...)
}

// Publish replaces the release list (a new ETag, so the next request is a 200).
func (s *Server) Publish(rels ...Release) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rels = rels
	tags := make([]string, len(rels))
	for i, r := range rels {
		tags[i] = r.Tag
	}
	s.etag = `"` + strings.Join(tags, "+") + `"`
}

// Counts reports the full list answers, the 304s and the artifact downloads.
func (s *Server) Counts() (lists, notModified, downloads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.notMod, s.downloads
}

// UserAgents lists the User-Agent of every list request, in order.
func (s *Server) UserAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.agents...)
}

// Hold stops every artifact download halfway until the returned func is called.
func (s *Server) Hold() (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold = ch
	s.mu.Unlock()
	return func() { close(ch) }
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = append(s.agents, r.Header.Get("User-Agent"))
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == s.etag {
		s.notMod++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.lists++
	type asset struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	}
	type rel struct {
		Tag       string    `json:"tag_name"`
		Published time.Time `json:"published_at"`
		Assets    []asset   `json:"assets"`
	}
	list := []rel{}
	for _, fr := range s.rels {
		r := rel{Tag: fr.Tag, Published: fr.Published, Assets: []asset{}}
		if fr.Art != nil {
			base := s.URL + "/dl/" + fr.Tag + "/"
			r.Assets = append(r.Assets,
				asset{Name: release.DataAsset, Size: int64(len(fr.Art.GZ)), URL: base + release.DataAsset},
				asset{Name: release.DataDigestAsset, Size: int64(len(fr.Art.Digest)), URL: base + release.DataDigestAsset})
		}
		list = append(list, r)
	}
	w.Header().Set("ETag", s.etag)
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	var art *Artifact
	for _, fr := range s.rels {
		if fr.Tag == r.PathValue("tag") {
			art = fr.Art
		}
	}
	if art != nil && name == release.DataAsset {
		s.downloads++
	}
	hold := s.hold
	s.mu.Unlock()
	switch {
	case art == nil:
		http.NotFound(w, r)
	case name == release.DataAsset && hold != nil:
		half := len(art.GZ) / 2
		_, _ = w.Write(art.GZ[:half])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-hold
		_, _ = w.Write(art.GZ[half:])
	case name == release.DataAsset:
		_, _ = w.Write(art.GZ)
	default:
		_, _ = io.WriteString(w, art.Digest)
	}
}
