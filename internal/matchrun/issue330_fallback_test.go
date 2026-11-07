package matchrun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

func TestIssue330UnknownIdentifierAnchorFallback(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	const path = "A/Book"
	e.book(t, path, "Book", "B0UNKNOWN1", nil)
	e.anchor(t, path, "old-slug", "bray")
	e.matcher.byWork["old-slug"] = []meta.MatchCandidate{workAnswer("new-slug", "bray")}
	run := e.start(t, catalog.MatchModeRefresh, "")
	items := e.items(t, run, "")
	if len(items) != 1 || items[0].Outcome != catalog.OutcomeAuto || !items[0].Changes[ScopeFill].Match {
		t.Fatalf("unknown identifier did not fall back to the recorded work: %+v", items)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if a := e.anchorOf(t, path); a.WorkID != "new-slug" {
		t.Errorf("retired work not healed: %+v", a)
	}
	if asin := e.field(t, path, catalog.FieldASIN); asin.Value != "B0UNKNOWN1" {
		t.Errorf("fallback replaced the book's own identifier: %+v", asin)
	}
}

type fallbackMirror struct{ handler http.Handler }

func (m fallbackMirror) Handler() http.Handler { return m.handler }
func (fallbackMirror) Ready() bool             { return true }

// Run the refresh against metaserve's real artifact and redirect handler, over
// HTTP and through the mirror transport. Only genuine identifier misses allow
// the recorded work lookup; a known ISBN after an unknown ASIN still wins.
func TestIssue330FallbackLookupPrecedence(t *testing.T) {
	t.Parallel()
	db, err := query.Open(querytest.Build(t, t.TempDir()), "fallback-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream := query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{})
	for _, mode := range []string{"online", "mirror"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, asin, isbn, anchor, wantWork, foundBy, outcome, detail string
				fallback                                                     bool
			}{
				{"unknown_asin_redirect", querytest.MissingASIN, "", querytest.RetiredWork, querytest.RetiredWorkTarget, "", catalog.OutcomeAuto, "", true},
				{"unknown_asin_live_work", querytest.MissingASIN, "", querytest.ASINWork, querytest.ASINWork, "", catalog.OutcomeAuto, "", true},
				{"unknown_isbn_redirect", "", "9780000000002", querytest.RetiredWork, querytest.RetiredWorkTarget, "", catalog.OutcomeAuto, "", true},
				{"both_unknown_redirect", querytest.MissingASIN, "9780000000002", querytest.RetiredWork, querytest.RetiredWorkTarget, "", catalog.OutcomeAuto, "", true},
				{"known_asin", querytest.ASIN, querytest.ISBN, querytest.RetiredWork, querytest.ASINWork, "asin:" + querytest.ASIN, catalog.OutcomeAuto, "", false},
				{"identifier_names_another_work", "", querytest.ISBN, querytest.RetiredWork, querytest.ISBNWork, "isbn:" + querytest.ISBN, catalog.OutcomeAuto, "", false},
				{"isbn_after_unknown_asin", querytest.MissingASIN, querytest.ISBN, querytest.RetiredWork, querytest.ISBNWork, "isbn:" + querytest.ISBN, catalog.OutcomeAuto, "", false},
				{"unknown_without_anchor", querytest.MissingASIN, "", "", "", "", catalog.OutcomeNone, DetailIdentifierUnknown, false},
				{"retired_without_redirect", querytest.MissingASIN, "", "retired-without-redirect", "", "", catalog.OutcomeNone, DetailWorkUnknown, true},
				{"identifier_outage", "B0ERROR001", querytest.ISBN, querytest.RetiredWork, "", "", catalog.OutcomeError, ErrCodeUnavailable, false},
				{"isbn_outage_after_asin_miss", querytest.MissingASIN, "9780000000003", querytest.RetiredWork, "", "", catalog.OutcomeError, ErrCodeUnavailable, false},
				{"recorded_work_outage", querytest.MissingASIN, "", "down", "", "", catalog.OutcomeError, ErrCodeUnavailable, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var mu sync.Mutex
					var paths []string
					h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						paths = append(paths, r.URL.Path)
						mu.Unlock()
						if r.URL.Path == "/api/v1/works/down" || (r.URL.Path == "/api/v1/lookup" &&
							(r.URL.Query().Get("asin") == "B0ERROR001" || r.URL.Query().Get("isbn") == "9780000000003")) {
							http.Error(w, "unavailable", http.StatusInternalServerError)
							return
						}
						upstream.ServeHTTP(w, r)
					})
					var remoteHits atomic.Int32
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						remoteHits.Add(1)
						h.ServeHTTP(w, r)
					}))
					t.Cleanup(remote.Close)
					svc := meta.NewService(remote.URL, nil)
					if mode == "mirror" {
						svc.SetMirror(fallbackMirror{h}, nil)
					}
					e := newEnv(t)
					ctx := context.Background()
					const path = "A/Book"
					b := &catalog.Book{LibraryID: e.lib, RelPath: path, IsFolder: true, Title: "Local",
						ASIN: tc.asin, ISBN: tc.isbn, AddedAt: "2020-01-01"}
					if _, err := e.cat.UpsertBook(ctx, b); err != nil {
						t.Fatal(err)
					}
					if tc.anchor != "" {
						e.anchor(t, path, tc.anchor, querytest.ASINRecording)
					}
					e.runner = New(e.cat, svc, e.runner.saveCover, e.enabled.Load, nil)
					run := e.start(t, catalog.MatchModeRefresh, "us")
					items := e.items(t, run, "")
					if len(items) != 1 || items[0].Outcome != tc.outcome || items[0].Detail != tc.detail {
						t.Fatalf("items = %+v, want %s / %s", items, tc.outcome, tc.detail)
					}
					mu.Lock()
					askedAnchor := tc.anchor != "" && slices.Contains(paths, "/api/v1/works/"+tc.anchor)
					askedFuzzy := slices.Contains(paths, "/api/v1/works/match") || slices.Contains(paths, "/api/v1/works/search")
					mu.Unlock()
					if askedAnchor != tc.fallback || askedFuzzy {
						t.Fatalf("requests = %v, want fallback %v, no fuzzy lookup", paths, tc.fallback)
					}
					// A local 500 may try the remote transport, which also returns
					// 500. Healthy mirror answers, including 404s and redirects, stay local.
					if mode == "mirror" && tc.outcome != catalog.OutcomeError && remoteHits.Load() != 0 {
						t.Fatalf("mirror asked the remote %d times", remoteHits.Load())
					}
					p := items[0].Proposal
					if tc.outcome == catalog.OutcomeAuto {
						if p.WorkID != tc.wantWork || items[0].Changes[ScopeFill].Match != (tc.anchor != tc.wantWork) {
							t.Fatalf("proposal = %+v, want healed match to %s", p, tc.wantWork)
						}
						wantFoundBy := tc.foundBy
						if tc.fallback {
							wantFoundBy = workFoundBy(tc.anchor, querytest.ASINRecording) + ";" + fallbackIdentifiers(tc.asin, tc.isbn)
						}
						if p.FoundBy != wantFoundBy || p.Values[catalog.FieldASIN] != "" || p.Values[catalog.FieldISBN] != "" || p.ASINRegion != "" {
							t.Fatalf("proposal = %+v, want found_by %s and no identifiers", p, wantFoundBy)
						}
					}
					if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
						t.Fatal(err)
					}
					e.runner.Wait()
					a := e.anchorOf(t, path)
					if tc.outcome == catalog.OutcomeAuto {
						if a.WorkID != tc.wantWork || a.How != catalog.MatchHowAccepted || e.field(t, path, catalog.FieldAuthor).Value == "" {
							t.Errorf("anchor = %+v, want healed and details refreshed", a)
						}
						if tc.fallback && a.RecordingID != querytest.ASINRecording {
							t.Errorf("fallback recording = %s, want the recorded one", a.RecordingID)
						}
					} else if a.WorkID != tc.anchor || e.field(t, path, catalog.FieldTitle).Value != "Local" {
						t.Errorf("failed lookup changed the book: anchor %+v", a)
					}
					if a, i := e.field(t, path, catalog.FieldASIN), e.field(t, path, catalog.FieldISBN); a.Value != tc.asin || i.Value != tc.isbn {
						t.Errorf("identifiers = %q / %q, want %q / %q", a.Value, i.Value, tc.asin, tc.isbn)
					}
				})
			}
		})
	}
}

// A fallback is valid only while both the identifiers that missed and the
// recorded work/recording stay unchanged, under either allowed scope.
func TestIssue330FallbackLeavesChangesSinceLookup(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{ScopeFill, ScopeOverwrite} {
		for _, since := range []string{"asin", "isbn_added", "isbn_changed", "work", "recording", "unlinked"} {
			t.Run(scope+"/"+since, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				ctx := context.Background()
				const path = "A/Book"
				e.book(t, path, "Book", "B0UNKNOWN1", no)
				if since == "isbn_changed" {
					if err := e.cat.SetEnrichment(ctx, e.lib, path, "", "9780000000002"); err != nil {
						t.Fatal(err)
					}
				}
				e.anchor(t, path, "old-slug", "bray")
				e.matcher.byWork["old-slug"] = []meta.MatchCandidate{workAnswer("new-slug", "bray")}
				run := e.start(t, catalog.MatchModeRefresh, "")
				if run.Counts.Auto != 1 {
					t.Fatalf("run = %+v, want a confident fallback", run)
				}
				edit := catalog.BookEdit{Source: catalog.SourceCommunity}
				switch since {
				case "asin":
					edit.Set = map[string]string{catalog.FieldASIN: "B0HAND0001"}
				case "isbn_added":
					edit.Set = map[string]string{catalog.FieldISBN: "9780000000002"}
				case "isbn_changed":
					edit.Set = map[string]string{catalog.FieldISBN: "9780000000003"}
				case "work":
					edit.Match = &catalog.MatchAnchor{WorkID: "hand-work", RecordingID: "bray"}
				case "recording":
					edit.Match = &catalog.MatchAnchor{WorkID: "old-slug", RecordingID: "hand-recording"}
				case "unlinked":
					edit.Match = &catalog.MatchAnchor{}
				}
				if err := e.cat.EditBook(ctx, e.lib, path, edit); err != nil {
					t.Fatal(err)
				}
				before := e.anchorOf(t, path)
				if ch := e.items(t, run, "")[0].Changes[scope]; len(ch.Fields) != 0 || ch.Match || ch.Cover {
					t.Fatalf("change after %s = %+v, want none", since, ch)
				}
				if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: scope}); err != nil {
					t.Fatal(err)
				}
				e.runner.Wait()
				if a := e.anchorOf(t, path); a != before || e.field(t, path, catalog.FieldNarrator).Value != "" || e.covers.Load() != 0 {
					t.Errorf("fallback wrote after %s: anchor %+v, before %+v, covers %d", since, a, before, e.covers.Load())
				}
				if it := e.items(t, run, "")[0]; it.Applied != catalog.ItemSkipped || it.Detail != "nothing_to_change" {
					t.Errorf("item = %+v, want skipped", it.MatchRunItem)
				}
			})
		}
	}
}

// A stopped apply can resume its cover after healing the recorded work, but an
// identifier changed since lookup invalidates that resume too (healedTo).
func TestIssue330FallbackHealStoppedInItsCover(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "identifier_changed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			ctx := context.Background()
			const path = "A/Book"
			e.book(t, path, "The Martian", "B0UNKNOWN1", no)
			e.anchor(t, path, "old-slug", "bray")
			e.matcher.byWork["old-slug"] = []meta.MatchCandidate{workAnswer("new-slug", "bray")}
			run := e.start(t, catalog.MatchModeRefresh, "")
			save := e.runner.saveCover
			e.runner.saveCover = func(ctx context.Context, _ int64, _, _ string, _ int64) error {
				e.runner.Cancel(run.ID)
				return ctx.Err()
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			if a := e.anchorOf(t, path); a.WorkID != "new-slug" {
				t.Fatalf("anchor = %+v, want healed before cover stopped", a)
			}
			if changed {
				if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{
					Set: map[string]string{catalog.FieldASIN: "B0HAND0001"}, Source: catalog.SourceCommunity,
				}); err != nil {
					t.Fatal(err)
				}
			}
			e.runner.saveCover = save
			if ch := e.items(t, run, "")[0].Changes[ScopeFill]; ch.Cover == changed || ch.Match {
				t.Fatalf("resume changes = %+v, identifier changed %v", ch, changed)
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			wantCovers, wantApplied := int32(1), catalog.ItemApplied
			if changed {
				wantCovers, wantApplied = 0, catalog.ItemSkipped
			}
			if it := e.items(t, run, "")[0]; it.Applied != wantApplied || e.covers.Load() != wantCovers {
				t.Errorf("resumed item = %+v, covers %d, want %s / %d", it.MatchRunItem, e.covers.Load(), wantApplied, wantCovers)
			}
		})
	}
}
