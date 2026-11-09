package api

import (
	"context"
	"database/sql"
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
	// A blank work id, one of invisible characters only (zero width space,
	// BOM, word joiner, Hangul filler, braille blank, a mix with spaces), or a
	// match naming no work at all, is a mistake, not the Unlink.
	for _, match := range []string{`{"work_id":" "}`, `{"work_id":"\t\n "}`, `{"work_id":"\u2003"}`,
		`{"work_id":"\u200b"}`, `{"work_id":"\ufeff"}`, `{"work_id":"\u2060"}`, `{"work_id":"\u3164"}`,
		`{"work_id":"\u2800"}`, `{"work_id":" \u200b\t\ufe0f "}`, `{"work_id":"\u200b","recording_id":"rec"}`,
		`{}`, `{"work_id":null}`, `{"recording_id":"rec"}`} {
		resp, body := e.do(t, "PATCH", url, admin,
			`{"match":`+match+`,"set":{"title":"Must not be written"}}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"field":"match"`) {
			t.Errorf("match %s: %d %s, want 400 on the match", match, resp.StatusCode, body)
		}
		st, err := e.cat.BookMatchState(ctx, libID, path)
		if err != nil || st.Anchor.WorkID != "artemis" || st.Title != "Community Title" {
			t.Fatalf("invalid unlink changed the book: %+v %v", st, err)
		}
		if _, err := e.cat.Cover(ctx, libID, path); err != nil {
			t.Fatalf("invalid unlink removed its cover: %v", err)
		}
	}
	// Invisible characters around a real id are trimmed; a non-ASCII id is
	// kept as it is.
	for in, want := range map[string]string{`\u200bdune\ufeff`: "dune", "münchhausen-é": "münchhausen-é"} {
		resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":"`+in+`"},"source":"community"}`)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"work_id":"`+want+`"`) {
			t.Fatalf("match %q = %d %s, want %q recorded", in, resp.StatusCode, body, want)
		}
	}
	// Only the explicitly empty id unlinks.
	if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
		t.Fatalf("explicit unlink = %d %s", resp.StatusCode, body)
	}
}

// unlinkFixture is the book an apply blocked on its cover fetch works on: the
// apply's field transaction is in, and the server's own cover saver
// (saveMatchCover) is held in its fetch until release.
type unlinkFixture struct {
	e       *testEnv
	admin   string
	libID   int64
	runID   int64
	path    string
	bookURL string
	release func()
	runner  *matchrun.Runner
}

const unlinkFixturePath = "Andy Weir/The Martian"

// newUnlinkFixture records a ready run (mode, over runLib: nil = every library)
// with one confident item for the book, which has no cover, proposing a narrator,
// a recorded match the-martian/rec and a cover, and applies it with fill until the
// cover fetch is entered. A refresh's book is matched to old-slug/rec beforehand
// (its item found by that match heals it).
func newUnlinkFixture(t *testing.T, mode string, runLib func(e *testEnv, libID int64) *int64) *unlinkFixture {
	t.Helper()
	e, admin, _, libID := newMatchRunEnv(t, "us")
	ctx := context.Background()
	const path = unlinkFixturePath
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
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	cover := testImage(t, 20, 20, true)
	e.api.fetchCover = func(context.Context, string, int64) ([]byte, error) {
		enterOnce.Do(func() { close(entered) })
		<-release
		return cover, nil
	}
	runner := matchrun.New(e.cat, nil, e.api.saveMatchCover, func() bool { return true }, nil)
	e.api.matchRuns = runner
	t.Cleanup(func() { unblock(); runner.Wait() })
	run := catalog.MatchRun{Mode: mode, Total: 1}
	if runLib != nil {
		run.LibraryID = runLib(e, libID)
	}
	id, err := e.cat.StartMatchRun(ctx, run)
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
	f := &unlinkFixture{e: e, admin: admin, libID: libID, runID: id, path: path, release: unblock, runner: runner,
		bookURL: fixtureBookURL(libID)}
	if resp, body := e.do(t, "POST", f.runURL()+"/apply", admin, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	waitFor(t, entered, "the apply to reach its cover")
	return f
}

func (f *unlinkFixture) runURL() string {
	return "/api/v1/admin/match-runs/" + strconv.FormatInt(f.runID, 10)
}

// finish releases the cover fetch and waits for the apply to end, returning the
// item as the apply marked it.
func (f *unlinkFixture) finish(t *testing.T) catalog.MatchRunItem {
	t.Helper()
	f.release()
	f.runner.Wait()
	items, _, err := f.e.cat.ListMatchRunItems(context.Background(), f.runID, "", 0, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	return items[0]
}

// coverSource is the source of the book's custom cover, "" for none.
func (f *unlinkFixture) coverSource(t *testing.T) string {
	t.Helper()
	var source string
	err := f.e.db.QueryRowContext(context.Background(),
		`SELECT source FROM book_covers WHERE library_id = ? AND path = ?`, f.libID, f.path).Scan(&source)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return source
}

// Block the cover after the apply's field transaction: an Unlink here used to
// remove the match and values successfully, then the cover was written back. It
// is refused while a run applies to the book's library, through the stopped
// apply's last cover write; a run over another library doesn't refuse it, and
// its cover is not written to the unlinked book (SetCoverIf).
func TestAdminUnlinkDuringActiveApply(t *testing.T) {
	t.Parallel()
	otherLibrary := func(e *testEnv, _ int64) *int64 {
		lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Other", Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return &lib.ID
	}
	sameLibrary := func(_ *testEnv, libID int64) *int64 { return &libID }
	for _, tc := range []struct {
		phase, mode string
		runLib      func(*testEnv, int64) *int64
	}{
		{"match", catalog.MatchModeMatch, nil},
		{"refresh", catalog.MatchModeRefresh, sameLibrary},
		{"stopping", catalog.MatchModeRefresh, nil},
		{"other library", catalog.MatchModeRefresh, otherLibrary},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()
			f := newUnlinkFixture(t, tc.mode, tc.runLib)
			e, admin, ctx := f.e, f.admin, context.Background()
			resp, body := e.do(t, "PATCH", f.bookURL, admin, `{"match":{"work_id":""},"set":{"title":"Kept by the refusal"}}`)
			if tc.phase == "other library" {
				if resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
					t.Fatalf("unlink during another library's apply = %d %s, want 200", resp.StatusCode, body)
				}
				it := f.finish(t)
				if it.Applied != catalog.ItemApplied || it.Detail != matchrun.DetailCoverSuperseded {
					t.Fatalf("item = %+v, want applied with its cover superseded", it)
				}
				st, err := e.cat.BookMatchState(ctx, f.libID, f.path)
				if err != nil || st.Anchor.WorkID != "" || st.Fields[catalog.FieldNarrator].Value != "" {
					t.Fatalf("book after the apply = %+v %v, want still unlinked", st, err)
				}
				if src := f.coverSource(t); src != "" {
					t.Fatalf("the apply wrote a %s cover to the unlinked book", src)
				}
				return
			}
			if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"match_run_busy"`) ||
				!strings.Contains(body, "applying to this library") {
				t.Fatalf("unlink during apply = %d %s, want 409 match_run_busy", resp.StatusCode, body)
			}
			st, err := e.cat.BookMatchState(ctx, f.libID, f.path)
			if err != nil || st.Anchor.WorkID != "the-martian" || st.Fields[catalog.FieldNarrator].Value != "Community Reader" || st.Title != "The Martian" {
				t.Fatalf("refused unlink changed the book: %+v %v", st, err)
			}
			if n, err := e.api.matchRuns.SettleOrphans(ctx); n != 0 || err != nil {
				t.Fatalf("settle during a live apply = %d %v, want nothing settled", n, err)
			}
			if tc.phase == "stopping" {
				if resp, body := e.do(t, "POST", f.runURL()+"/cancel", admin, ""); resp.StatusCode != http.StatusNoContent {
					t.Fatalf("stop = %d %s", resp.StatusCode, body)
				}
				if resp, body := e.do(t, "PATCH", f.bookURL, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"match_run_busy"`) {
					t.Fatalf("unlink before the stopped worker ends = %d %s", resp.StatusCode, body)
				}
				if n, err := e.api.matchRuns.SettleOrphans(ctx); n != 0 || err != nil {
					t.Fatalf("settle during a stopped apply's cover = %d %v, want nothing settled", n, err)
				}
				if run, err := e.cat.GetMatchRun(ctx, f.runID); err != nil || run.Status != catalog.MatchApplying {
					t.Fatalf("stopped run before its worker ends = %+v %v, want still applying", run, err)
				}
			}
			f.finish(t)
			if tc.phase != "stopping" && f.coverSource(t) != catalog.SourceCommunity {
				t.Fatal("apply lost its cover")
			}
			if resp, body := e.do(t, "PATCH", f.bookURL, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
				t.Fatalf("unlink after apply = %d %s", resp.StatusCode, body)
			}
			st, err = e.cat.BookMatchState(ctx, f.libID, f.path)
			if err != nil || st.Fields[catalog.FieldNarrator].Value != "" {
				t.Fatalf("unlink left community values: %+v %v", st, err)
			}
			if _, err := e.cat.Cover(ctx, f.libID, f.path); err != catalog.ErrNotFound {
				t.Fatalf("unlink left the community cover: %v", err)
			}
		})
	}
}

// A run that only matches (records items) doesn't refuse an Unlink, nor does
// one over another library, nor a run that is ready, applied or interrupted.
func TestAdminUnlinkWhileMatching(t *testing.T) {
	t.Parallel()
	e, admin, _, libID := newMatchRunEnv(t, "us")
	ctx := context.Background()
	const path = unlinkFixturePath
	url := matchedBookURL(t, e, libID)
	if _, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: catalog.MatchModeMatch, LibraryID: &libID}); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
		t.Fatalf("unlink while a run matches = %d %s, want 200", resp.StatusCode, body)
	}
	if st, err := e.cat.BookMatchState(ctx, libID, path); err != nil || st.Anchor.WorkID != "" || st.Fields[catalog.FieldNarrator].Value != "" {
		t.Fatalf("book after the unlink = %+v %v", st, err)
	}
}

// A slow community cover never lands over a newer cover or match: the book
// changed while the apply fetched it (an upload, a match accepted by hand) keeps
// what it has, the item noted cover_superseded; an unchanged book gets its cover.
func TestMatchApplyCoverSuperseded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f *unlinkFixture)
		source string // the custom cover's source after the apply
		detail string
	}{
		{"upload", func(t *testing.T, f *unlinkFixture) {
			if err := f.e.cat.SetCover(context.Background(), f.libID, f.path, testImage(t, 30, 30, false), 0, catalog.SourceEdited); err != nil {
				t.Fatal(err)
			}
		}, catalog.SourceEdited, matchrun.DetailCoverSuperseded},
		{"matched again", func(t *testing.T, f *unlinkFixture) {
			resp, body := f.e.do(t, "PATCH", f.bookURL, f.admin,
				`{"match":{"work_id":"another-work","recording_id":"other"},"set":{"narrator":"Other Reader"},"source":"community"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("accept = %d %s", resp.StatusCode, body)
			}
		}, "", matchrun.DetailCoverSuperseded},
		{"asin set", func(t *testing.T, f *unlinkFixture) {
			resp, body := f.e.do(t, "PATCH", f.bookURL, f.admin, `{"set":{"asin":"B0MINE0001"}}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("edit = %d %s", resp.StatusCode, body)
			}
		}, "", matchrun.DetailCoverSuperseded},
		{"unchanged", func(*testing.T, *unlinkFixture) {}, catalog.SourceCommunity, ""},
		{"another field edited", func(t *testing.T, f *unlinkFixture) {
			if resp, body := f.e.do(t, "PATCH", f.bookURL, f.admin, `{"set":{"title":"My Title"}}`); resp.StatusCode != http.StatusOK {
				t.Fatalf("edit = %d %s", resp.StatusCode, body)
			}
		}, catalog.SourceCommunity, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnlinkFixture(t, catalog.MatchModeMatch, nil)
			tc.change(t, f)
			it := f.finish(t)
			if it.Applied != catalog.ItemApplied || it.Detail != tc.detail {
				t.Fatalf("item = %+v, want applied with detail %q", it, tc.detail)
			}
			if src := f.coverSource(t); src != tc.source {
				t.Fatalf("cover source = %q, want %q", src, tc.source)
			}
		})
	}
}
