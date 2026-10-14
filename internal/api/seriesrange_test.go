package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// omnibusMetaserve is a metadata site whose Saga rail lists the omnibus of books
// 1-3 beside them: one@1, two@2, three@3, omni@1-3, four@4. Only B0ONE is known.
func omnibusMetaserve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asin") != "B0ONE" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"one","title":"one","authors":[]},"recording_id":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "one" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"one","title":"one","authors":[],"language":"en","series":[{"id":"saga","name":"Saga","position":"1"}],"recordings":[]}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"saga","name":"Saga","authors":[],"works":[` +
			`{"position":"1","work":{"id":"one","title":"One","authors":[]}},` +
			`{"position":"2","work":{"id":"two","title":"Two","authors":[]}},` +
			`{"position":"3","work":{"id":"three","title":"Three","authors":[]}},` +
			`{"position":"1-3","work":{"id":"omni","title":"Books 1-3","authors":[]}},` +
			`{"position":"4","work":{"id":"four","title":"Four","authors":[]}}]}`))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	return mock
}

// TestSeriesRangeWire: a book at a range carries series_index (its start) and
// series_index_end on every player book shape (item, list), a series_list entry
// at a range position_end, and neither appears for a book without one. Its next
// book follows the range's end, and a community rail entry at the same range
// holds it.
func TestSeriesRangeWire(t *testing.T) {
	t.Parallel()
	e := newLibraryEnv(t, omnibusMetaserve(t), true,
		&catalog.Book{RelPath: "Saga/1", Series: "Saga", SeriesIndex: 1, ASIN: "B0ONE"},
		&catalog.Book{RelPath: "Saga/2", Series: "Saga", SeriesIndex: 2},
		&catalog.Book{RelPath: "Saga/Omnibus", Series: "Saga", SeriesIndex: 1},
		&catalog.Book{RelPath: "Saga/4", Series: "Saga", SeriesIndex: 4},
	)
	ctx := context.Background()
	for path, set := range map[string]map[string]string{
		"Saga/Omnibus": {catalog.FieldSeriesIndex: "1-3"},
		"Saga/2":       {catalog.FieldMoreSeries: `[{"name":"Box","position":1,"position_end":2}]`},
	} {
		if err := e.cat.EditBook(ctx, e.lib.ID, path, catalog.BookEdit{Set: set}); err != nil {
			t.Fatal(err)
		}
	}
	item := func(path string) string {
		t.Helper()
		resp, body := e.do(t, "GET", e.url("item", path), e.adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("item %s = %d %s", path, resp.StatusCode, body)
		}
		return body
	}
	if body := item("Saga/Omnibus"); !strings.Contains(body, `"series":"Saga","series_index":1,"series_index_end":3,`) {
		t.Errorf("omnibus item = %s, want series_index 1 and series_index_end 3", body)
	}
	body := item("Saga/2")
	if strings.Contains(body, "series_index_end") ||
		!strings.Contains(body, `"series_list":[{"name":"Saga","position":2},{"name":"Box","position":1,"position_end":2}]`) {
		t.Errorf("Saga/2 item = %s, want no series_index_end and Box at 1-2", body)
	}
	// The list shape carries it too.
	resp, list := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/books?series=Saga", e.adminTok, "")
	if resp.StatusCode != http.StatusOK || strings.Count(list, `"series_index_end":3`) != 1 {
		t.Errorf("books = %d %s, want the omnibus' series_index_end once", resp.StatusCode, list)
	}
	// The next book after the omnibus is after its end.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/Omnibus"), e.adminTok); n.Source != nextSeries || n.nextPath() != "Saga/4" {
		t.Errorf("next after the omnibus = %s", n.raw)
	}
	// On the rail: one by its work, the omnibus on the entry at its range (by
	// index, its work unknown), and Saga/4 on 4. Entry 2 is Saga/2's.
	if got, want := railLocals(t, e, "Saga/1", e.adminTok), map[string]string{
		"one": "Saga/1", "two": "Saga/2", "three": "", "omni": "Saga/Omnibus", "four": "Saga/4",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("rail locals = %v, want %v", got, want)
	}
	// The series aggregate (a player's gaps) holds every slot of the omnibus.
	var series struct {
		Series []catalog.SeriesCount `json:"series"`
	}
	resp, raw := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/series", e.adminTok, "")
	if err := json.Unmarshal([]byte(raw), &series); resp.StatusCode != http.StatusOK || err != nil || len(series.Series) == 0 {
		t.Fatalf("series = %d %s", resp.StatusCode, raw)
	}
	for _, s := range series.Series {
		if s.Name == "Saga" && !reflect.DeepEqual(s.Positions, []float64{1, 2, 3, 4}) {
			t.Errorf("Saga positions = %v, want 1-4", s.Positions)
		}
	}
}
