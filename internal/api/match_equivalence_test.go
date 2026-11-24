package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/matchrun"
)

// seriesMetaserve is matchRunMetaserve's community with the work in two series,
// Discworld #8 then City Watch #1, and no language (so no series facts asked).
func seriesMetaserve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(strings.Join(r.URL.Query()["title"], " "), "Martian") {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		if r.URL.Query().Get("asin") == "B0SHORT001" {
			_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"the-martian","recording_id":"abridged","score":100,"reasons":{"title":1,"identifier":"asin"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"the-martian","score":96,"reasons":{"title":1}}]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],
			"description":"Stranded.","first_published":"2011",
			"series":[{"id":"discworld","name":"Discworld","position":"8"},{"id":"city-watch","name":"City Watch","position":"1"}],
			"recordings":[{"id":"bray","narrators":[{"id":"rcb","name":"R. C. Bray"}],"publisher":"Podium",
			"runtime_min":634,"asin":[{"region":"uk","asin":"B0UK000001"}],"isbn":[]},
			{"id":"abridged","abridged":true,"runtime_min":335,"asin":[{"region":"uk","asin":"B0SHORT001"}]}]}`))
	})
	mux.HandleFunc("GET /api/v1/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func bookEditURL(lib int64, path string) string {
	return "/api/v1/admin/libraries/" + strconv.FormatInt(lib, 10) + "/book?path=" + url.QueryEscape(path)
}

// TestMatchDialogAcceptEqualsRunApply: the match dialog's accept with its
// default ticks (the proposal's set and revert, the match beside them) writes
// what an overwrite apply of a match run writes for the same book and
// candidate: the admin's edited title kept, the book's own main series (one of
// the work's) kept with its community position, the work's other series as
// more_series, the same values, provenance and recorded match.
func TestMatchDialogAcceptEqualsRunApply(t *testing.T) {
	t.Parallel()
	const path = "Andy Weir/The Martian"
	setup := func(t *testing.T) (*testEnv, string, int64) {
		base := seriesMetaserve(t)
		e := newTestEnvWith(t, func(c *config.Config) {
			c.Metadata.Enabled, c.Metadata.BaseURL, c.Metadata.Region = true, base, "uk"
		})
		admin, _, _ := adminAndMember(t, e)
		ctx := context.Background()
		lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		b := catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "The Martian", IsFolder: true, Author: "Andy Weir",
			Series: "City Watch", SeriesIndex: 4, Narrator: "Someone", Duration: 634 * 60, TagASIN: "B0SHORT001", AddedAt: "2020-01-01"}
		if _, err := e.cat.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
		// The admin's own title: neither the dialog's defaults nor a run take it over.
		edit := bookEditURL(lib.ID, path)
		if resp, body := e.do(t, "PATCH", edit, admin, `{"set":{"title":"The Martian (mine)"}}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("edit title = %d %s", resp.StatusCode, body)
		}
		return e, admin, lib.ID
	}

	// A run over the library, applied under overwrite.
	run, runAdmin, runLib := setup(t)
	resp, body := run.do(t, "POST", "/api/v1/admin/match-runs", runAdmin, `{"library_id":`+strconv.FormatInt(runLib, 10)+`}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(decodeRun(t, body).ID, 10)
	run.api.matchRuns.Wait()
	_, body = run.do(t, "GET", runURL+"/items?outcome=auto", runAdmin, "")
	var page struct {
		Items []struct {
			Proposal struct {
				RecordingID string `json:"recording_id"`
			} `json:"proposal"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || len(page.Items) != 1 || page.Items[0].Proposal.RecordingID != "bray" {
		t.Fatalf("items = %s", body)
	}
	recording := page.Items[0].Proposal.RecordingID
	if resp, body := run.do(t, "POST", runURL+"/apply", runAdmin, `{"scope":"overwrite"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	run.api.matchRuns.Wait()

	// The same book in the dialog: its proposal for the run's candidate, accepted as planned.
	dialog, admin, lib := setup(t)
	matchURL := "/api/v1/admin/libraries/" + strconv.FormatInt(lib, 10) + "/book/match?path=" + url.QueryEscape(path)
	for _, tc := range []struct{ query, want string }{
		{"", recording}, {"&q=Martian&asin=B0SHORT001", "abridged"},
	} {
		resp, out := dialog.do(t, "GET", matchURL+tc.query, admin, "")
		var result struct {
			Candidates []struct {
				Default string `json:"default_recording_id"`
				Hit     string `json:"recording_id"`
			} `json:"candidates"`
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("match dialog %q: %d %s", tc.query, resp.StatusCode, out)
		}
		decodeInto(t, out, &result)
		if len(result.Candidates) != 1 || result.Candidates[0].Default != tc.want || result.Candidates[0].Hit != "abridged" {
			t.Fatalf("match dialog %q = %s, want default %q for abridged hit", tc.query, out, tc.want)
		}
	}
	_, body = dialog.do(t, "GET", "/api/v1/admin/books/match-proposal?library_id="+strconv.FormatInt(lib, 10)+
		"&path="+url.QueryEscape(path)+"&work_id=the-martian&recording_id="+recording, admin, "")
	var p matchrun.BookProposal
	if err := json.Unmarshal([]byte(body), &p); err != nil || !p.Match {
		t.Fatalf("proposal = %s", body)
	}
	accept, _ := json.Marshal(map[string]any{
		"set": p.Set, "revert": p.Revert, "source": "community",
		"match": map[string]string{"work_id": p.WorkID, "recording_id": p.RecordingID,
			"asin": p.Values[catalog.FieldASIN], "isbn": p.Values[catalog.FieldISBN]},
	})
	if resp, body := dialog.do(t, "PATCH", bookEditURL(lib, path), admin, string(accept)); resp.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d %s", resp.StatusCode, body)
	}

	ctx := context.Background()
	got, err := dialog.cat.BookMatchState(ctx, lib, path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := run.cat.BookMatchState(ctx, runLib, path)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(m map[string]catalog.FieldValue) map[string]catalog.FieldValue {
		out := maps.Clone(m)
		for k, v := range out {
			v.EditedAt, v.EditedBy = "", ""
			out[k] = v
		}
		return out
	}
	if !maps.Equal(strip(got.Fields), strip(want.Fields)) {
		t.Fatalf("dialog fields = %+v\nrun fields = %+v", got.Fields, want.Fields)
	}
	if got.Anchor.WorkID != want.Anchor.WorkID || got.Anchor.RecordingID != want.Anchor.RecordingID ||
		got.Anchor.OfferedASIN != want.Anchor.OfferedASIN || got.Anchor.OfferedISBN != want.Anchor.OfferedISBN {
		t.Fatalf("dialog match = %+v, run match = %+v", got.Anchor, want.Anchor)
	}
	// And what both wrote is the plan the test is about.
	f := got.Fields
	if f[catalog.FieldTitle].Value != "The Martian (mine)" || f[catalog.FieldTitle].Source != catalog.SourceEdited ||
		f[catalog.FieldSeries].Value != "City Watch" || f[catalog.FieldSeriesIndex].Value != "1" ||
		f[catalog.FieldSeriesIndex].Source != catalog.SourceCommunity ||
		!strings.Contains(f[catalog.FieldMoreSeries].Value, "Discworld") || strings.Contains(f[catalog.FieldMoreSeries].Value, "City Watch") ||
		f[catalog.FieldNarrator].Value != "R. C. Bray" || f[catalog.FieldNarrator].Source != catalog.SourceCommunity ||
		f[catalog.FieldASIN].Value != "B0UK000001" || got.Anchor.WorkID != "the-martian" {
		t.Fatalf("written = %+v, match %+v", f, got.Anchor)
	}
}
