package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/meta"
)

// remoteService is a metaserve: GETs answered by get (nil: 500, counted
// either way), the intake POST recorded and answered 202.
type remoteService struct {
	*httptest.Server
	gets  atomic.Int32
	mu    sync.Mutex
	posts [][]string
}

func newRemote(t *testing.T, get http.HandlerFunc) *remoteService {
	t.Helper()
	rs := &remoteService{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/intake/unknown-asins" {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				ASINs []string `json:"asins"`
			}
			_ = json.Unmarshal(body, &req)
			rs.mu.Lock()
			rs.posts = append(rs.posts, req.ASINs)
			rs.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"accepted":1,"known":0,"invalid":0}`))
			return
		}
		rs.gets.Add(1)
		if get == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		get(w, r)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *remoteService) posted() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []string
	for _, p := range rs.posts {
		out = append(out, p...)
	}
	return out
}

// Remote mode, end to end through meta.Service: the lookups and the POST go to
// the service, and only the unknown ASINs are sent.
func TestReportRemoteMode(t *testing.T) {
	t.Parallel()
	rs := newRemote(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/v1/lookup" && r.URL.Query().Get("asin") == "B0KNOWN001":
			_, _ = w.Write([]byte(`{"work":{"id":"w1","title":"T"},"recording_id":"r1"}`))
		default:
			http.NotFound(w, r)
		}
	})
	e := newEnv(t)
	e.book(t, "known", "B0KNOWN001", false)
	e.book(t, "unknown", "b0unknown1", false)
	r := New(e.cat, meta.NewService(rs.URL, nil), Options{On: e.on.Load, Now: e.clock.now})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rs.posted(); !slices.Equal(got, []string{"B0UNKNOWN1"}) {
		t.Fatalf("posted %v", got)
	}
	if n := rs.gets.Load(); n != 3 { // healthz + two lookups
		t.Fatalf("%d GETs, want 3", n)
	}
}

// localCopy is a ready local copy: metaserve's own handler over the querytest
// artifact.
type localCopy struct{ h http.Handler }

func (c localCopy) Handler() http.Handler { return c.h }
func (c localCopy) Ready() bool           { return true }

// localHandler is the local copy's API over a fresh test database.
func localHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := query.Open(querytest.Build(t, t.TempDir()), "data-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{})
}

// breakingCopy is a local copy that breaks after its first lookup: from then
// on it answers 500 (drop false) or is no longer Ready (drop true, the copy
// dropped mid-run).
type breakingCopy struct {
	h       http.Handler
	drop    bool
	lookups atomic.Int32
	broken  atomic.Bool
}

func (c *breakingCopy) Ready() bool { return !c.drop || !c.broken.Load() }

func (c *breakingCopy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/lookup" {
			c.lookups.Add(1)
			if c.broken.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer c.broken.Store(true)
		}
		c.h.ServeHTTP(w, r)
	})
}

// Mirror mode, the copy failing or dropped mid-run: the lookups never go to
// the remote service; the first failed one stops the checking (failed, as any
// failed lookup), and the one the copy confirmed unknown is still sent.
func TestReportMirrorModeStaysLocal(t *testing.T) {
	t.Parallel()
	for _, drop := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop=%v", drop), func(t *testing.T) {
			t.Parallel()
			rs := newRemote(t, func(w http.ResponseWriter, _ *http.Request) {
				// Were it asked, every ASIN would be known.
				_, _ = w.Write([]byte(`{"work":{"id":"w1","title":"T"},"recording_id":"r1"}`))
			})
			svc := meta.NewService(rs.URL, nil)
			cp := &breakingCopy{h: localHandler(t), drop: drop}
			svc.SetMirror(cp, nil)

			e := newEnv(t)
			unknown := e.unknowns(t, 3)
			r := New(e.cat, svc, Options{On: e.on.Load, Now: e.clock.now, Ready: cp.Ready})
			if err := r.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce = nil, want the failed lookup")
			}
			if n := rs.gets.Load(); n != 0 {
				t.Fatalf("mirror mode asked the remote service %d GETs", n)
			}
			wantLookups := int32(2)
			if drop {
				wantLookups = 1 // the second never reaches a copy that is not Ready
			}
			if n := cp.lookups.Load(); n != wantLookups {
				t.Fatalf("the copy was asked %d lookups, want %d (the checking stops at the failure)", n, wantLookups)
			}
			rows, _ := e.cat.UnknownASINRows(context.Background())
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want the one lookup the copy answered", rows)
			}
			posted := rs.posted()
			if len(posted) != 1 || rows[posted[0]].CheckedAt == "" || !slices.Contains(unknown, posted[0]) {
				t.Fatalf("posted %v, want the one ASIN the copy confirmed unknown", posted)
			}
			if st := e.status(t); st.State != StateFailed || st.LastSent != 1 {
				t.Fatalf("status = %+v, want failed with 1 sent", st)
			}
		})
	}
}

// Mirror mode: the health check and the lookups are answered by the local
// copy (the remote service is never asked one), and the POST reaches the
// remote service.
func TestReportMirrorMode(t *testing.T) {
	t.Parallel()
	rs := newRemote(t, nil)
	svc := meta.NewService(rs.URL, nil)
	svc.SetMirror(localCopy{localHandler(t)}, nil)

	e := newEnv(t)
	e.book(t, "known", querytest.ASIN, false)
	e.book(t, "missing", querytest.MissingASIN, false)
	e.book(t, "other", "B0NOTINDB1", true)
	r := New(e.cat, svc, Options{On: e.on.Load, Now: e.clock.now, Ready: func() bool { return true }})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rs.gets.Load(); n != 0 {
		t.Fatalf("mirror mode asked the remote service %d GETs", n)
	}
	if got := sorted(rs.posted()); !slices.Equal(got, []string{querytest.MissingASIN, "B0NOTINDB1"}) {
		t.Fatalf("posted %v", got)
	}
}
