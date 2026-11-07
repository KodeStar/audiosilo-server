package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/matchrun"
)

func TestAdminUnlinkRefusesWhitespaceWorkID(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	admin, _, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	const path = "Andy Weir/Artemis"
	ctx := context.Background()
	url := base + "/book?path=" + escape(path)
	if err := e.cat.EditBook(ctx, libID, path, catalog.BookEdit{
		Set:   map[string]string{catalog.FieldTitle: "Community Title"},
		Match: &catalog.MatchAnchor{WorkID: "artemis"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	cover := testImage(t, 20, 20, true)
	if err := e.cat.SetCover(ctx, libID, path, cover, 0, catalog.SourceCommunity); err != nil {
		t.Fatal(err)
	}
	for _, work := range []string{`" "`, `"\t\n "`, `"\u2003"`} {
		resp, body := e.do(t, "PATCH", url, admin,
			`{"match":{"work_id":`+work+`},"set":{"title":"Must not be written"}}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("work_id %s: %d %s, want 400", work, resp.StatusCode, body)
		}
		st, err := e.cat.BookMatchState(ctx, libID, path)
		if err != nil || st.Anchor.WorkID != "artemis" || st.Title != "Community Title" {
			t.Fatalf("invalid unlink changed the book: %+v %v", st, err)
		}
		if _, err := e.cat.Cover(ctx, libID, path); err != nil {
			t.Fatalf("invalid unlink removed its cover: %v", err)
		}
	}
	// Only the explicitly empty id unlinks.
	if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
		t.Fatalf("explicit unlink = %d %s", resp.StatusCode, body)
	}
}

// Block the cover after the apply's field transaction: an Unlink here used to
// remove the match and values successfully, then the cover was written back.
func TestAdminUnlinkDuringActiveApply(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{catalog.MatchModeMatch, catalog.MatchModeRefresh, "stopping"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			mode := phase
			if phase == "stopping" {
				mode = catalog.MatchModeRefresh
			}
			e, admin, _, libID := newMatchRunEnv(t, "us")
			ctx := context.Background()
			const path = "Andy Weir/The Martian"
			b, err := e.cat.GetBookByPath(ctx, libID, path)
			if err != nil {
				t.Fatal(err)
			}
			b.HasCover = new(false)
			if _, err := e.cat.UpsertBook(ctx, b); err != nil {
				t.Fatal(err)
			}
			if mode == catalog.MatchModeRefresh {
				if err := e.cat.EditBook(ctx, libID, path, catalog.BookEdit{
					Match: &catalog.MatchAnchor{WorkID: "old-slug", RecordingID: "rec"}, Source: catalog.SourceCommunity,
				}); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			cover := testImage(t, 20, 20, true)
			runner := matchrun.New(e.cat, nil, func(ctx context.Context, id int64, path, _ string, user int64) error {
				close(entered)
				<-release
				return e.cat.SetCover(ctx, id, path, cover, user, catalog.SourceCommunity)
			}, func() bool { return true }, nil)
			e.api.matchRuns = runner
			t.Cleanup(func() { unblock(); runner.Wait() })
			id, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: mode, Total: 1})
			if err != nil {
				t.Fatal(err)
			}
			foundBy := ""
			if mode == catalog.MatchModeRefresh {
				foundBy = "work:old-slug/rec"
			}
			if err := e.cat.RecordMatchItem(ctx, id, &catalog.MatchRunItem{
				LibraryID: libID, Path: path, Outcome: catalog.OutcomeAuto,
				Proposal: catalog.MatchProposal{WorkID: "the-martian", RecordingID: "rec", FoundBy: foundBy,
					Values: map[string]string{catalog.FieldNarrator: "Community Reader"}, CoverURL: "https://example.com/cover.png"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.cat.FinishMatching(ctx, id, catalog.MatchReady, ""); err != nil {
				t.Fatal(err)
			}
			applyURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(id, 10) + "/apply"
			if resp, body := e.do(t, "POST", applyURL, admin, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
				t.Fatalf("apply = %d %s", resp.StatusCode, body)
			}
			<-entered
			url := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book?path=" + escape(path)
			resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""},"set":{"title":"Must not be written"}}`)
			if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"match_run_busy"`) {
				t.Fatalf("unlink during apply = %d %s, want 409 match_run_busy", resp.StatusCode, body)
			}
			st, err := e.cat.BookMatchState(ctx, libID, path)
			if err != nil || st.Anchor.WorkID != "the-martian" || st.Fields[catalog.FieldNarrator].Value != "Community Reader" || st.Title != "The Martian" {
				t.Fatalf("refused unlink changed the book: %+v %v", st, err)
			}
			if phase == "stopping" {
				stopURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(id, 10) + "/cancel"
				if resp, body := e.do(t, "POST", stopURL, admin, ""); resp.StatusCode != http.StatusNoContent {
					t.Fatalf("stop = %d %s", resp.StatusCode, body)
				}
				if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"match_run_busy"`) {
					t.Fatalf("unlink before the stopped worker ends = %d %s", resp.StatusCode, body)
				}
			}
			unblock()
			runner.Wait()
			if _, err := e.cat.Cover(ctx, libID, path); err != nil && phase != "stopping" {
				t.Fatalf("apply lost its cover: %v", err)
			}
			if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
				t.Fatalf("unlink after apply = %d %s", resp.StatusCode, body)
			}
			st, err = e.cat.BookMatchState(ctx, libID, path)
			if err != nil || st.Fields[catalog.FieldNarrator].Value != "" {
				t.Fatalf("unlink left community values: %+v %v", st, err)
			}
			if _, err := e.cat.Cover(ctx, libID, path); err != catalog.ErrNotFound {
				t.Fatalf("unlink left the community cover: %v", err)
			}
		})
	}
}
