package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestAdminMatchListsTheRecordedMatch (server#331 item 5): the console's
// default match search lists the book's recorded match, last and marked
// recorded, when its own search misses it; never twice, never beside typed
// text, and not at all when the community no longer has the work. Admin only.
func TestAdminMatchListsTheRecordedMatch(t *testing.T) {
	t.Parallel()
	m := &mockMetaserve{lookupCode: http.StatusNotFound, match: true, matchBody: `{"results":[]}`}
	e := newMetaEnvMock(t, true, m)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	const path = "Andy Weir/Artemis"
	record := func(work string) {
		t.Helper()
		if err := e.cat.EditBook(context.Background(), libID, path, catalog.BookEdit{
			Match: &catalog.MatchAnchor{WorkID: work, RecordingID: "rec1"}, Source: catalog.SourceCommunity,
		}); err != nil {
			t.Fatal(err)
		}
	}
	type candidate struct {
		WorkID      string `json:"work_id"`
		RecordingID string `json:"recording_id"`
		Recorded    bool   `json:"recorded"`
	}
	search := func(query string) []candidate {
		t.Helper()
		resp, body := e.do(t, "GET", base+"/book/match?path="+escape(path)+query, adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("search %q = %d %s", query, resp.StatusCode, body)
		}
		var out struct{ Candidates []candidate }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Candidates
	}
	if got := search(""); len(got) != 0 {
		t.Fatalf("no recorded match = %+v, want the search's own (none)", got)
	}
	record("the-martian")
	if got := search(""); len(got) != 1 || got[0].WorkID != "the-martian" || got[0].RecordingID != "rec1" || !got[0].Recorded {
		t.Fatalf("missed recorded match = %+v, want it listed, recorded", got)
	}
	if got := search("&q=artemis"); len(got) != 0 {
		t.Fatalf("typed search = %+v, want only what it found", got)
	}
	// Found by the search itself: listed once, as the search has it.
	setMatchBody(m, "")
	if got := search(""); len(got) != 1 || got[0].Recorded {
		t.Fatalf("found recorded match = %+v, want it once, not marked", got)
	}
	setMatchBody(m, `{"results":[]}`)
	record("gone-work") // the community no longer has it (404)
	if got := search(""); len(got) != 0 {
		t.Fatalf("a gone recorded work = %+v, want nothing added", got)
	}
	if resp, _ := e.do(t, "GET", base+"/book/match?path="+escape(path), memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member search = %d, want 403", resp.StatusCode)
	}
}

// TestAdminBookShowsTheEnrichmentUnderAMatch (server#350 item 1): the book
// page's ASIN field says which Bridge enrichment a community value hides, so
// the Unlink confirm knows the book stays matched by it. Admin only.
func TestAdminBookShowsTheEnrichmentUnderAMatch(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	ctx := context.Background()
	const path = "Andy Weir/Artemis"
	if err := e.cat.BackfillASIN(ctx, libID, path, "B0ENRICHED"); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.EditBook(ctx, libID, path, catalog.BookEdit{
		Set: map[string]string{catalog.FieldASIN: "B0MATCHED1"}, Match: &catalog.MatchAnchor{WorkID: "artemis"},
		Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "GET", base+"/book?path="+escape(path), adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("book = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Fields map[string]catalog.FieldValue `json:"fields"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if f := out.Fields[catalog.FieldASIN]; f.Value != "B0MATCHED1" || f.Source != catalog.SourceCommunity || !f.Locked ||
		f.Enrichment != "B0ENRICHED" {
		t.Fatalf("asin field = %+v, want the community value over the enrichment", f)
	}
	if f := out.Fields[catalog.FieldISBN]; f.Enrichment != "" {
		t.Fatalf("isbn field = %+v, want no enrichment", f)
	}
	if resp, _ := e.do(t, "GET", base+"/book?path="+escape(path), memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member book = %d, want 403", resp.StatusCode)
	}
}

// TestAdminMatchRunItemsSayEveryHold (server#352): the review's items carry
// every reason a review item is held for (holds, its detail the first), and an
// item with one reason (as every item stored before) has its detail alone.
// Admin only.
func TestAdminMatchRunItemsSayEveryHold(t *testing.T) {
	t.Parallel()
	e, adminTok, memberTok, libID := newMatchRunEnv(t, "uk")
	ctx := context.Background()
	runID, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: catalog.MatchModeMatch, LibraryID: &libID, Total: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []*catalog.MatchRunItem{
		{LibraryID: libID, Path: "Andy Weir/The Martian", Outcome: catalog.OutcomeReview, Score: 95,
			Detail: "identifier_disagrees", Holds: []string{"identifier_disagrees", "length_differs"},
			Proposal: catalog.MatchProposal{WorkID: "the-martian", Values: map[string]string{}}},
		{LibraryID: libID, Path: "Someone/Obscure", Outcome: catalog.OutcomeReview, Score: 90, Detail: "length_differs",
			Proposal: catalog.MatchProposal{WorkID: "obscure", Values: map[string]string{}}},
	} {
		if err := e.cat.RecordMatchItem(ctx, runID, it); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cat.FinishMatching(ctx, runID, catalog.MatchReady, ""); err != nil {
		t.Fatal(err)
	}
	itemsURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(runID, 10) + "/items?outcome=review"
	resp, body := e.do(t, "GET", itemsURL, adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("items = %d %s", resp.StatusCode, body)
	}
	var page struct {
		Items []struct {
			Path   string   `json:"path"`
			Detail string   `json:"detail"`
			Holds  []string `json:"holds"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, it := range page.Items {
		got[it.Path] = [2]string{it.Detail, strings.Join(it.Holds, ",")}
	}
	if got["Andy Weir/The Martian"] != [2]string{"identifier_disagrees", "identifier_disagrees,length_differs"} ||
		got["Someone/Obscure"] != [2]string{"length_differs", ""} || strings.Count(body, `"holds"`) != 1 {
		t.Fatalf("items = %s, want both reasons on the first and holds omitted on the second", body)
	}
	if resp, _ := e.do(t, "GET", itemsURL, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member items = %d, want 403", resp.StatusCode)
	}
}

func setMatchBody(m *mockMetaserve, body string) {
	m.mu.Lock()
	m.matchBody = body
	m.mu.Unlock()
}
