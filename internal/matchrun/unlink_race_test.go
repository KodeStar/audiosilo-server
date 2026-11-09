package matchrun

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// TestUnlinkRacesApply runs an Unlink of a book against the apply of a run over
// its library, again and again, under -race. The run's item is recorded before
// the Unlink, so an apply that begins after it finds the book no longer has the
// match it was found by and writes nothing. Whenever the Unlink went through,
// once the apply is done the book has no recorded match, no community value
// and no community cover: the Unlink's check shares its writer transaction with
// the clear, BeginApply is serialized with it, and the cover is written only to
// the book it was planned for (SetCoverIf).
func TestUnlinkRacesApply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const path = "A/a"
	e.book(t, path, "Book", "", no)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	e.runner.saveCover = func(ctx context.Context, lib int64, p, _ string, user int64, expect catalog.CoverExpect) error {
		time.Sleep(3 * time.Millisecond) // the fetch: an Unlink may land meanwhile
		return e.cat.SetCoverIf(ctx, lib, p, png, user, catalog.SourceCommunity, &expect)
	}
	var unlinked, refused int
	for i := range 40 {
		if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{
			Set:   map[string]string{catalog.FieldNarrator: "Community Reader"},
			Match: &catalog.MatchAnchor{WorkID: "w", RecordingID: "r"}, Source: catalog.SourceCommunity,
		}); err != nil {
			t.Fatal(err)
		}
		lib := e.lib
		id, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: catalog.MatchModeRefresh, LibraryID: &lib, Total: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.cat.RecordMatchItem(ctx, id, &catalog.MatchRunItem{
			LibraryID: e.lib, Path: path, Outcome: catalog.OutcomeAuto,
			Proposal: catalog.MatchProposal{WorkID: "w", RecordingID: "r", FoundBy: "work:w/r",
				Values: map[string]string{catalog.FieldNarrator: "Fresh Reader"}, CoverURL: "https://c/x.jpg"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.cat.FinishMatching(ctx, id, catalog.MatchReady, ""); err != nil {
			t.Fatal(err)
		}
		var (
			wg            sync.WaitGroup
			unlinkErr     error
			applyErr      error
			go1, go2      = make(chan struct{}), make(chan struct{})
			startTogether = func(mine chan struct{}, other chan struct{}) { close(mine); <-other }
		)
		wg.Go(func() {
			startTogether(go1, go2)
			_, applyErr = e.runner.Apply(ctx, ctx, id, ApplyOptions{Scope: ScopeOverwrite})
		})
		wg.Go(func() {
			startTogether(go2, go1)
			// From before the apply begins to after it ends.
			time.Sleep(time.Duration(i%8) * 700 * time.Microsecond)
			unlinkErr = e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{Match: &catalog.MatchAnchor{}})
		})
		wg.Wait()
		e.runner.Wait()
		if applyErr != nil {
			t.Fatalf("round %d: apply = %v", i, applyErr)
		}
		switch {
		case errors.Is(unlinkErr, catalog.ErrMatchRunBusy):
			refused++
		case unlinkErr != nil:
			t.Fatalf("round %d: unlink = %v", i, unlinkErr)
		default:
			unlinked++
			st, err := e.cat.BookMatchState(ctx, e.lib, path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Anchor.WorkID != "" {
				t.Fatalf("round %d: unlinked book has a match again: %+v", i, st.Anchor)
			}
			for f, v := range st.Fields {
				if v.Source == catalog.SourceCommunity {
					t.Fatalf("round %d: unlinked book has a community %s %q", i, f, v.Value)
				}
			}
			if _, err := e.cat.Cover(ctx, e.lib, path); !errors.Is(err, catalog.ErrNotFound) {
				t.Fatalf("round %d: unlinked book has a cover: %v", i, err)
			}
		}
		// The next round starts from a book with no cover.
		if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{Match: &catalog.MatchAnchor{}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("unlinks: %d went through, %d refused", unlinked, refused)
}

// A cover the book no longer takes (catalog.ErrCoverSuperseded) is final: an
// item that wrote fields is applied, one that only wanted the cover skipped,
// both with detail cover_superseded, even when a stop lands meanwhile (never
// cover_pending, which the next apply would take again).
func TestCoverSupersededIsFinal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fields  bool
		stop    bool
		applied string
	}{
		{"cover only", false, false, catalog.ItemSkipped},
		{"with fields", true, false, catalog.ItemApplied},
		{"with fields, stopped", true, true, catalog.ItemApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.book(t, "A/a", "Book", "", no)
			e.matcher.byTitle["Book"] = []meta.MatchCandidate{cand("the-martian", 97, "https://c/rec.jpg")}
			run := e.start(t, catalog.MatchModeMatch, "")
			if !tc.fields {
				// Matched since, by the run's own values, so only the cover is left:
				// planned as a cover_pending item is.
				it := e.items(t, run, catalog.OutcomeAuto)[0]
				save := e.runner.saveCover
				e.runner.saveCover = func(ctx context.Context, _ int64, _, _ string, _ int64, _ catalog.CoverExpect) error {
					_, _ = e.runner.Cancel(context.Background(), run.ID)
					return ctx.Err()
				}
				if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
					t.Fatal(err)
				}
				e.runner.Wait()
				e.runner.saveCover = save
				if got := e.items(t, run, catalog.OutcomeAuto)[0]; got.Detail != DetailCoverPending || got.ID != it.ID {
					t.Fatalf("item = %+v, want cover_pending", got.MatchRunItem)
				}
			}
			var expect catalog.CoverExpect
			e.runner.saveCover = func(_ context.Context, _ int64, _, _ string, _ int64, x catalog.CoverExpect) error {
				expect = x
				if tc.stop {
					_, _ = e.runner.Cancel(context.Background(), run.ID)
				}
				return catalog.ErrCoverSuperseded
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			it := e.items(t, run, catalog.OutcomeAuto)[0]
			if it.Applied != tc.applied || it.Detail != DetailCoverSuperseded {
				t.Fatalf("item = %+v, want %s with cover_superseded", it.MatchRunItem, tc.applied)
			}
			if want := it.Proposal; expect.WorkID != want.WorkID || expect.RecordingID != want.RecordingID || expect.ASIN != want.Values[catalog.FieldASIN] {
				t.Fatalf("expect = %+v, want the proposal's match and ASIN (%+v)", expect, want)
			}
		})
	}
}
