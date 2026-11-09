package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestMatchAcceptOfferedIdentifiers (server#330 item 1): the match dialog's
// accept sends the identifiers the match offered, ticked or not (match.asin,
// match.isbn). One unticked is recorded as offered, so a later refresh by the
// recorded match never writes it; an accept that sends none (an older console)
// fills as before. Denied: an offered identifier the field would refuse is 400
// on field "match", and the book is unchanged.
func TestMatchAcceptOfferedIdentifiers(t *testing.T) {
	t.Parallel()
	e, tok, _, libID := newMatchRunEnv(t, "uk")
	ctx := context.Background()
	lib := strconv.FormatInt(libID, 10)
	bookURL := func(path string) string { return "/api/v1/admin/libraries/" + lib + "/book?path=" + escape(path) }
	const unticked, older = "Andy Weir/The Martian", "Someone/Obscure"

	// Denied first.
	for _, match := range []string{`{"work_id":"the-martian","asin":"not-an-asin"}`, `{"work_id":"the-martian","isbn":"12345"}`} {
		resp, body := e.do(t, "PATCH", bookURL(unticked), tok,
			`{"set":{"narrator":"Must not be written"},"source":"community","match":`+match+`}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"field":"match"`) {
			t.Errorf("match %s = %d %s, want 400 on the match", match, resp.StatusCode, body)
		}
	}
	if st, err := e.cat.BookMatchState(ctx, libID, unticked); err != nil || st.Anchor.WorkID != "" ||
		st.Fields[catalog.FieldNarrator].Value != "" {
		t.Fatalf("a refused accept changed the book: %+v %v", st, err)
	}

	// The ASIN unticked: every other value taken, the ASIN only offered.
	accept := `{"set":{"narrator":"R. C. Bray","description":"Stranded."},"source":"community",` +
		`"match":{"work_id":"the-martian","recording_id":"bray"%s}}`
	for path, offered := range map[string]string{unticked: `,"asin":"b0uk000001"`, older: ""} {
		if resp, body := e.do(t, "PATCH", bookURL(path), tok, strings.Replace(accept, "%s", offered, 1)); resp.StatusCode != http.StatusOK {
			t.Fatalf("accept %s = %d %s", path, resp.StatusCode, body)
		}
	}
	if st, _ := e.cat.BookMatchState(ctx, libID, unticked); st.Anchor.OfferedASIN != "B0UK000001" || st.Fields[catalog.FieldASIN].Value != "" {
		t.Fatalf("unticked = anchor %+v asin %+v, want the ASIN offered, not written", st.Anchor, st.Fields[catalog.FieldASIN])
	}

	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", tok, `{"mode":"refresh"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	e.api.matchRuns.Wait()
	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10)
	_, body = e.do(t, "GET", runURL+"/items?outcome=auto", tok, "")
	if strings.Contains(body, `"path":"`+unticked+`"`) || !strings.Contains(body, `"path":"`+older+`"`) {
		t.Fatalf("refresh items = %s, want only the book that offered none", body)
	}
	if resp, body = e.do(t, "POST", runURL+"/apply", tok, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	if b, err := e.cat.GetBookByPath(ctx, libID, unticked); err != nil || b.ASIN != "" {
		t.Errorf("unticked book = %+v %v, want no ASIN", b, err)
	}
	if b, err := e.cat.GetBookByPath(ctx, libID, older); err != nil || b.ASIN != "B0UK000001" {
		t.Errorf("older accept = %+v %v, want the ASIN filled as before", b, err)
	}
}

// TestRefreshApplyLeavesOutUnchanged (server#332): a refresh apply with no
// exclusions leaves out an item with nothing to change under the scope: the
// run ends applied, its total counting only the books changed, the item still
// pending and not skipped.
func TestRefreshApplyLeavesOutUnchanged(t *testing.T) {
	t.Parallel()
	e, tok, _, libID := newMatchRunEnv(t, "uk")
	ctx := context.Background()
	if err := e.cat.SetEnrichment(ctx, libID, "Someone/Obscure", "B0US000001", ""); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", tok, `{"mode":"refresh"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	e.api.matchRuns.Wait()
	// Fill has the narrator to write; once it is there, nothing (the title is
	// the book's own, and only overwrite would replace it).
	if err := e.cat.EditBook(ctx, libID, "Someone/Obscure", catalog.BookEdit{
		Set: map[string]string{catalog.FieldNarrator: "R. C. Bray", catalog.FieldDescription: "Stranded."}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10)
	if resp, body = e.do(t, "POST", runURL+"/apply", tok, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	_, body = e.do(t, "GET", runURL, tok, "")
	if got := decodeRun(t, body); got.Status != catalog.MatchApplied || got.ApplyTotal != 0 || got.ApplyDone != 0 ||
		got.Counts.Skipped != 0 || got.Counts.Applied != 0 || got.Counts.Pending != 1 {
		t.Fatalf("run = %+v, want applied with the unchanged item left out", got)
	}
}
