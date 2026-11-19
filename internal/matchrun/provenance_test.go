package matchrun

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// provenance is the book's recorded match's how, run, editor and time, as
// stored (run and editor NULL as 0).
type provenance struct {
	how          string
	runID, setBy int64
	setAt        string
}

func (e *env) provenance(t *testing.T, path string) provenance {
	t.Helper()
	var p provenance
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT how, COALESCE(run_id, 0), COALESCE(set_by, 0), set_at FROM book_matches WHERE library_id = ? AND path = ?`,
		e.lib, path).Scan(&p.how, &p.runID, &p.setBy, &p.setAt); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRefreshAndRepickKeepTheMatchsProvenance (server#355 items 1-2): a refresh
// that fills an identifier, and a repick that switches the ASIN, rewrite the
// book's own match without taking it over: how, the run that made it, who and
// when ("matched on") stay. A hand re-accept of the same match made while the
// refresh applies keeps the re-accept's own provenance.
func TestRefreshAndRepickKeepTheMatchsProvenance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	other, err := auth.New(e.db, time.Now).CreateUser(ctx, "other", "correct horse battery", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// A store listing matched by hand, offering no identifier; its recording
	// gained an ASIN since, which a refresh fills.
	e.book(t, "A/Filled", "The Martian", "", nil)
	e.book(t, "A/Reaccepted", "The Martian", "", nil)
	for _, p := range []string{"A/Filled", "A/Reaccepted"} {
		if err := e.cat.EditBook(ctx, e.lib, p, catalog.BookEdit{
			Match: &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray"}, Source: catalog.SourceCommunity, UserID: e.admin,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A book a match run (77) applied with a US ASIN, which a repick switches.
	e.book(t, "A/Repicked", "Repicked", "", nil)
	if err := e.cat.EditBook(ctx, e.lib, "A/Repicked", catalog.BookEdit{
		Set:    map[string]string{catalog.FieldASIN: "B0US000001"},
		Match:  &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray", How: catalog.MatchHowAuto, RunID: 77, OfferedASIN: "B0US000001"},
		Source: catalog.SourceCommunity, UserID: e.admin,
	}); err != nil {
		t.Fatal(err)
	}
	before := map[string]provenance{}
	for _, p := range []string{"A/Filled", "A/Reaccepted", "A/Repicked"} {
		before[p] = e.provenance(t, p)
	}

	e.matcher.byWork["the-martian"] = []meta.MatchCandidate{workAnswer("the-martian", "bray")}
	refresh := e.start(t, catalog.MatchModeRefresh, "uk")
	if refresh.Counts.Auto != 3 {
		t.Fatalf("refresh = %+v, want all three (the repick's book by its match too)", refresh)
	}
	var reaccepted provenance
	e.runner.beforeEdit = func(it *catalog.MatchRunItem) {
		if it.Path != "A/Reaccepted" {
			return
		}
		// The same match accepted again by another admin, as the apply has read it.
		if err := e.cat.EditBook(ctx, e.lib, it.Path, catalog.BookEdit{
			Match: &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray"}, Source: catalog.SourceCommunity, UserID: other.ID,
		}); err != nil {
			t.Error(err)
		}
		reaccepted = e.provenance(t, it.Path)
	}
	if _, err := e.runner.Apply(ctx, ctx, refresh.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	e.runner.beforeEdit = nil
	if a := e.field(t, "A/Filled", catalog.FieldASIN); a.Value != "B0UK000001" {
		t.Fatalf("filled asin = %+v", a)
	}
	if a := e.anchorOf(t, "A/Filled"); a.OfferedASIN != "B0UK000001" {
		t.Fatalf("filled anchor = %+v, want the ASIN recorded as offered", a)
	}
	if got := e.provenance(t, "A/Filled"); got != before["A/Filled"] {
		t.Errorf("filled provenance = %+v, want %+v kept", got, before["A/Filled"])
	}
	if reaccepted.setBy != other.ID {
		t.Fatalf("re-accept = %+v, want made by the other admin", reaccepted)
	}
	if got := e.provenance(t, "A/Reaccepted"); got != reaccepted {
		t.Errorf("re-accepted provenance = %+v, want the hand re-accept's %+v kept", got, reaccepted)
	}

	hit := cand("the-martian", 100, "")
	hit.RecordingID = "bray"
	e.matcher.byASIN["B0US000001"] = []meta.MatchCandidate{hit}
	repick := e.start(t, catalog.MatchModeRepick, "uk")
	if items := e.items(t, repick, ""); len(items) != 1 || items[0].Path != "A/Repicked" {
		t.Fatalf("repick items = %+v", items)
	}
	if _, err := e.runner.Apply(ctx, ctx, repick.ID, ApplyOptions{Scope: ScopeIDs, UserID: other.ID}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if a := e.anchorOf(t, "A/Repicked"); a.OfferedASIN != "B0UK000001" || e.field(t, "A/Repicked", catalog.FieldASIN).Value != "B0UK000001" {
		t.Fatalf("repicked anchor = %+v, want the UK ASIN written and offered", a)
	}
	if got := e.provenance(t, "A/Repicked"); got != before["A/Repicked"] || got.runID != 77 {
		t.Errorf("repicked provenance = %+v, want run 77's %+v kept", got, before["A/Repicked"])
	}
}

// TestMatchRecordsEveryHoldReason (server#352 item 1, server#448): an item held
// for more than one reason records them all, the first its detail, so a tag
// identifier that disagrees with a recording of another length says both; an
// item with one reason has no list.
func TestMatchRecordsEveryHoldReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.book(t, "A/Both", "Both", "", nil)   // 634 min
	e.book(t, "A/Short", "Short", "", nil) // 634 min
	both := cand("the-martian", 100, "")
	title := 0.5
	both.Reasons = &meta.MatchReasons{Identifier: "asin", Title: &title}
	both.Recordings[0].RuntimeMin = 300
	e.matcher.byTitle["Both"] = []meta.MatchCandidate{both}
	short := cand("the-martian", 100, "")
	short.Recordings[0].RuntimeMin = 300
	e.matcher.byTitle["Short"] = []meta.MatchCandidate{short}

	run := e.start(t, catalog.MatchModeMatch, "")
	review := e.items(t, run, catalog.OutcomeReview)
	slices.SortFunc(review, func(a, b ItemView) int { return strings.Compare(a.Path, b.Path) })
	if len(review) != 2 {
		t.Fatalf("review = %+v, want both held", review)
	}
	if it := review[0]; it.Path != "A/Both" || it.Detail != DetailIdentifierDisagrees ||
		!slices.Equal(it.Holds, []string{DetailIdentifierDisagrees, DetailLengthDiffers}) {
		t.Errorf("both = %q %v, want identifier_disagrees then length_differs", it.Detail, it.Holds)
	}
	if it := review[1]; it.Path != "A/Short" || it.Detail != DetailLengthDiffers || it.Holds != nil {
		t.Errorf("short = %q %v, want length_differs alone", it.Detail, it.Holds)
	}
}

// TestMatchAsksAgainPastAWrongIdentifier (server#352 item 3): a hint whose work
// the book disagrees with (a wrong volume's ASIN) is asked again without it,
// and another work that query finds is the item, held for review with
// identifier_elsewhere however confident; when it finds the same work, or
// nothing worth a look, or fails, the hint's work stays held as before. A hint
// the book agrees with is asked once.
func TestMatchAsksAgainPastAWrongIdentifier(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	wrong := func(work string) meta.MatchCandidate {
		c := cand(work, 100, "")
		title := 0.4
		c.Reasons = &meta.MatchReasons{Identifier: "asin", Title: &title, Series: "conflict"}
		return c
	}
	right := cand("right-volume", 97, "")
	right.Title = "Right Volume"
	byTitle := map[string]meta.MatchCandidate{"Elsewhere": right, "Same": cand("wrong-volume", 97, ""),
		"Weak": cand("weak", 30, ""), "Agrees": cand("agrees", 97, "")}
	byTitle["Weak"] = func() meta.MatchCandidate {
		c := byTitle["Weak"]
		c.Reasons = &meta.MatchReasons{}
		return c
	}()
	e.matcher.answer = func(q meta.MatchQuery) []meta.MatchCandidate {
		switch {
		case q.BookASIN == "" && q.Title == "Nothing":
			return nil
		case q.BookASIN == "":
			return []meta.MatchCandidate{byTitle[q.Title]}
		case q.Title == "Agrees":
			c := cand("agrees", 100, "")
			c.Reasons = &meta.MatchReasons{Identifier: "asin"}
			return []meta.MatchCandidate{c}
		}
		return []meta.MatchCandidate{wrong("wrong-volume")}
	}
	e.matcher.failWhen = func(q meta.MatchQuery) bool { return q.BookASIN == "" && q.Title == "Fails" }
	for _, title := range []string{"Elsewhere", "Same", "Weak", "Nothing", "Fails", "Agrees"} {
		e.book(t, "A/"+title, title, "", nil)
		if _, err := e.db.ExecContext(context.Background(), `UPDATE books SET tag_asin = ? WHERE rel_path = ?`,
			"B0TAG"+title[:3], "A/"+title); err != nil {
			t.Fatal(err)
		}
	}
	run := e.start(t, catalog.MatchModeMatch, "")
	got := map[string]ItemView{}
	for _, it := range e.items(t, run, "") {
		got[it.Path] = it
	}
	if it := got["A/Elsewhere"]; it.Outcome != catalog.OutcomeReview || it.Proposal.WorkID != "right-volume" ||
		it.Detail != DetailIdentifierElsewhere || it.Score != 97 {
		t.Errorf("elsewhere = %s %q %q %d, want the title's work held as identifier_elsewhere",
			it.Outcome, it.Proposal.WorkID, it.Detail, it.Score)
	}
	for _, p := range []string{"A/Same", "A/Weak", "A/Nothing", "A/Fails"} {
		if it := got[p]; it.Outcome != catalog.OutcomeReview || it.Proposal.WorkID != "wrong-volume" ||
			it.Detail != DetailIdentifierDisagrees {
			t.Errorf("%s = %s %q %q, want the hint's work held as identifier_disagrees", p, it.Outcome, it.Proposal.WorkID, it.Detail)
		}
	}
	if it := got["A/Agrees"]; it.Outcome != catalog.OutcomeAuto || it.Proposal.WorkID != "agrees" {
		t.Errorf("agrees = %s %q, want the hint's work confident", it.Outcome, it.Proposal.WorkID)
	}
	asked := map[string]int{}
	for _, q := range e.matcher.queries {
		asked[q.Title]++
	}
	for title, want := range map[string]int{"Elsewhere": 2, "Same": 2, "Weak": 2, "Nothing": 2, "Fails": 2, "Agrees": 1} {
		if asked[title] != want {
			t.Errorf("%s asked %d times, want %d (the disagreeing hints twice, the agreeing once)", title, asked[title], want)
		}
	}
}

// TestUnknownASINRunAsksItsOwnASINOnce: an unknown_asin run asks by the book's
// own ASIN, no hint, so a record that ASIN names now is not asked again
// without it, whatever the title says (the apply passes such a book by:
// asinKnownSince); the item stays the ASIN's work, held as before.
func TestUnknownASINRunAsksItsOwnASINOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Own", "Own", "B0NOPE0007", nil)
	if err := e.cat.SaveASINChecks(ctx, []catalog.ASINCheck{{ASIN: "B0NOPE0007", Unknown: true}}, "2026-11-01T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	e.matcher.answer = func(q meta.MatchQuery) []meta.MatchCandidate {
		if q.BookASIN == "" {
			return []meta.MatchCandidate{cand("title-work", 97, "")}
		}
		c := cand("wrong-volume", 100, "")
		title := 0.4
		c.Reasons = &meta.MatchReasons{Identifier: "asin", Title: &title, Series: "conflict"}
		return []meta.MatchCandidate{c}
	}
	run := e.start(t, catalog.MatchModeUnknownASIN, "uk")
	items := e.items(t, run, catalog.OutcomeReview)
	if len(items) != 1 || items[0].Proposal.WorkID != "wrong-volume" || items[0].Detail != DetailIdentifierDisagrees {
		t.Fatalf("items = %+v, want the ASIN's work held as identifier_disagrees", items)
	}
	if n := len(e.matcher.queries); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
}
