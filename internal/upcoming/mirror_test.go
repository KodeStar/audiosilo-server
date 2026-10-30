package upcoming

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/mirrortest"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// localCopy is a ready local copy: metaserve's own handler over the querytest
// artifact.
type localCopy struct{ h http.Handler }

func (c localCopy) Handler() http.Handler { return c.h }
func (c localCopy) Ready() bool           { return true }

// In mirror mode a whole run (the work lookup, the work's series, the watch
// feed, the series read) is answered by the local copy: the remote service is
// never asked.
func TestRunnerMirrorModeAsksNoRemote(t *testing.T) {
	t.Parallel()
	db, err := query.Open(querytest.Build(t, t.TempDir()), "data-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	remote, hits := mirrortest.Remote(t)
	svc := meta.NewService(remote.URL, nil)
	svc.SetMirror(localCopy{query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{})}, nil)

	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), nil)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	work, err := svc.Work(ctx, querytest.SeriesWork)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := svc.WorkSeries(ctx, work.ID)
	if err != nil || len(refs) == 0 {
		t.Fatalf("WorkSeries = %v, %v", refs, err)
	}
	if _, err := cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: "S/1", Title: work.Title, Author: "A",
		Series: refs[0].Name, SeriesIndex: 1, ISBN: querytest.ISBN, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	r := New(cat, svc, Options{Now: time.Now})
	if err := r.RunOnce(ctx, true); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("mirror mode asked the remote service %d times", n)
	}
	stored, err := cat.MetaSeriesAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := stored[querytest.SeriesID]; !ok || len(rec.Members) != querytest.SeriesWorks {
		t.Fatalf("the series must be read from the copy: %+v", stored)
	}
	st, _ := r.Status(ctx)
	if st.LastSuccessAt == "" || st.Series != 1 {
		t.Fatalf("status = %+v", st)
	}
}
