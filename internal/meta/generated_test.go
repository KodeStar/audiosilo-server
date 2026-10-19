package meta

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func testGenerated() *Generated {
	return &Generated{
		Characters: &model.Characters{Characters: []model.Character{
			{ID: "local-early", Name: "Early", Reveal: model.Position{Chapter: 1}},
			{ID: "local-late", Name: "Late", Reveal: model.Position{Chapter: 5}},
		}},
		Recaps: &model.Recaps{Recaps: []model.Recap{
			{Through: model.Position{Chapter: 0}, Text: "Local before."},
			{Through: model.Position{Chapter: 3}, Text: "Local three."},
		}, InShort: "Local short.", Ending: "Local ending."},
		GeneratedAt: "2026-10-15T10:00:00Z",
	}
}

func communityWork() *MetaWork {
	return &MetaWork{ID: "w", Title: "W",
		Characters:  []MetaCharacter{{ID: "community-hero", Name: "Hero", Reveal: MetaPosition{Chapter: 1}}},
		Attribution: &MetaAttribution{Credit: attributionContributors}}
}

// matched is Answer's envelope for a matched book.
func matched(t *testing.T, env *Enrichment, g *Generated, hide *Place) *Enrichment {
	t.Helper()
	out, ok := Answer(env, "The Book", g, hide).(*Enrichment)
	if !ok {
		t.Fatalf("a matched book answered %T", out)
	}
	return out
}

// TestAnswerFillsWhatTheCommunityLacks: community wins per dimension; generated
// recaps (and their summary) fill a work without any; the cached envelope is
// never written to.
func TestAnswerFillsWhatTheCommunityLacks(t *testing.T) {
	t.Parallel()
	env := &Enrichment{Matched: true, Work: communityWork()}
	snapshot, _ := json.Marshal(env)
	w := matched(t, env, testGenerated(), nil).Work
	if len(w.Characters) != 1 || w.Characters[0].ID != "community-hero" {
		t.Errorf("characters = %+v, want the community's", w.Characters)
	}
	if len(w.Recaps) != 2 || w.Recaps[0].Text != "Local before." || w.RecapSummary == nil || w.RecapSummary.Ending != "Local ending." {
		t.Errorf("recaps = %+v, summary = %+v, want the local ones", w.Recaps, w.RecapSummary)
	}
	if g := w.Generated; g == nil || g.Characters || !g.Recaps || !g.RecapSummary || g.GeneratedAt != "2026-10-15T10:00:00Z" {
		t.Errorf("generated = %+v", g)
	}
	if w.Attribution == nil {
		t.Error("the community characters lost their attribution")
	}
	if after, _ := json.Marshal(env); string(after) != string(snapshot) {
		t.Errorf("the shared envelope changed:\n%s\n%s", snapshot, after)
	}

	// Hidden at the start: the community hero (chapter 1) stays with its credit,
	// the generated recap at chapter 0 stays marked, no summary.
	hidden := matched(t, env, testGenerated(), &Place{}).Work
	if hidden.Attribution == nil || hidden.Generated == nil || !hidden.Generated.Recaps || hidden.Generated.RecapSummary || len(hidden.Recaps) != 1 || hidden.RecapSummary != nil {
		t.Errorf("hidden = %+v / %+v", hidden, hidden.Generated)
	}
	// Community characters all gated away: the credit goes, and the generated
	// characters don't step in (the community owns that dimension).
	late := &Enrichment{Work: &MetaWork{ID: "w", Characters: []MetaCharacter{{ID: "c", Reveal: MetaPosition{Chapter: 9}}},
		Attribution: &MetaAttribution{}}}
	gated := matched(t, late, testGenerated(), &Place{}).Work
	if gated.Attribution != nil || len(gated.Characters) != 0 || gated.Generated == nil || gated.Generated.Characters {
		t.Errorf("gated = %+v / %+v", gated, gated.Generated)
	}

	// A work with everything from the community keeps it: nothing generated, and
	// nothing to load (Fillable).
	both := communityWork()
	both.Recaps = []MetaRecap{{Through: MetaPosition{Chapter: 1}, Text: "Community."}}
	both.CommunityDescription = &MetaCommunityDescription{Text: "Community description."}
	env = &Enrichment{Matched: true, Work: both}
	if out := matched(t, env, withDescription(testGenerated()), nil); out != env || out.Work.Generated != nil {
		t.Errorf("all community = %+v", out.Work)
	}
	// Characters, recaps and a description but no summary: still nothing to
	// fill, as a summary only ever comes with the recaps.
	if got := env.Fillable(); got.Any() {
		t.Errorf("a work with characters, recaps and a description fills %+v", got)
	}
	// Without the community description there is one thing to fill, and only
	// that part is loaded.
	noDesc := *both
	noDesc.CommunityDescription = nil
	if got := (&Enrichment{Work: &noDesc}).Fillable(); got != (metadata.AttachmentParts{Description: true}) {
		t.Errorf("a work without a community description fills %+v", got)
	}
	if got := (&Enrichment{Work: &MetaWork{Characters: both.Characters}}).Fillable(); got != (metadata.AttachmentParts{Recaps: true, Description: true}) {
		t.Errorf("a work with characters alone fills %+v", got)
	}
	if got := (*Enrichment)(nil).Fillable(); got != metadata.AllAttachmentParts {
		t.Errorf("no match fills %+v", got)
	}

	// A community summary stays beside generated recaps, and still counts as
	// community content.
	summed := &MetaWork{ID: "w", RecapSummary: &MetaRecapSummary{InShort: "Community short."}, Attribution: &MetaAttribution{}}
	out := matched(t, &Enrichment{Work: summed}, testGenerated(), nil)
	if out.Work.RecapSummary.InShort != "Community short." || out.Work.Attribution == nil || len(out.Work.Recaps) != 2 {
		t.Errorf("community summary = %+v", out.Work)
	}
	// So the recaps are loaded for it: a summary alone leaves the recap list to
	// fill, and the estimate's coverage agrees (Generate is offered for it).
	if got := (&Enrichment{Work: summed}).Fillable(); got != metadata.AllAttachmentParts {
		t.Errorf("a work with a summary alone fills %+v", got)
	}
	if cov := coverageOf(&Enrichment{Work: summed}); cov.Recaps || !cov.Known {
		t.Errorf("a work with a summary alone covers %+v, want no recaps", cov)
	}
	if cov := coverageOf(&Enrichment{Work: &MetaWork{Recaps: []MetaRecap{{Text: "x"}}}}); !cov.Recaps {
		t.Errorf("a work with a recap list covers %+v, want recaps", cov)
	}
	// The community summary keeps its source: only the list is marked generated.
	if g := out.Work.Generated; g == nil || !g.Recaps || g.RecapSummary {
		t.Errorf("generated mark beside a community summary = %+v", g)
	}
}

// TestAnswerGeneratedAt: the mark is dated by what it names: the characters'
// and recaps' run, or the description's own time when that is all it names (a
// description-only job keeps the run's time in bridge.json).
func TestAnswerGeneratedAt(t *testing.T) {
	t.Parallel()
	g := withDescription(testGenerated())
	g.DescriptionAt = "2026-10-16T08:00:00Z"
	full := communityWork()
	full.Recaps = []MetaRecap{{Through: MetaPosition{Chapter: 1}, Text: "Community."}}
	if at := matched(t, &Enrichment{Matched: true, Work: full}, g, nil).Work.Generated.GeneratedAt; at != g.DescriptionAt {
		t.Errorf("description alone dated %q, want %q", at, g.DescriptionAt)
	}
	if at := matched(t, &Enrichment{Matched: true, Work: communityWork()}, g, nil).Work.Generated.GeneratedAt; at != g.GeneratedAt {
		t.Errorf("recaps and a description dated %q, want the run's %q", at, g.GeneratedAt)
	}
	u, ok := Answer(nil, "The Book", &Generated{Description: g.Description, DescriptionAt: g.DescriptionAt}, nil).(Unmatched)
	if !ok || u.Work == nil || u.Work.Generated.GeneratedAt != g.DescriptionAt {
		t.Errorf("unmatched description alone = %+v", u.Work)
	}
}

// testDescription is a generated description (its bounds are the parser's,
// not Answer's).
const testDescription = "A harbour town on the edge of a long winter."

// withDescription is g with a generated description.
func withDescription(g *Generated) *Generated {
	g.Description = &model.Description{Work: "w", Text: testDescription, License: "CC-BY-SA-4.0"}
	return g
}

// TestAnswerDescription: a generated description fills generated_description
// (never community_description) only when the community work has no community
// description; the core description doesn't block it; it survives
// spoilers=hide (spoiler-free by contract); it never brings an attribution.
func TestAnswerDescription(t *testing.T) {
	t.Parallel()
	// Matched, with community characters and recaps but no community
	// description: the description alone is generated.
	full := communityWork()
	full.Recaps = []MetaRecap{{Through: MetaPosition{Chapter: 1}, Text: "Community."}}
	full.Description = "The publisher's blurb."
	env := &Enrichment{Matched: true, Work: full}
	snapshot, _ := json.Marshal(env)
	w := matched(t, env, withDescription(testGenerated()), nil).Work
	if w.GeneratedDescription == nil || w.GeneratedDescription.Text != testDescription || w.CommunityDescription != nil ||
		w.Description != "The publisher's blurb." {
		t.Fatalf("description = %+v / %+v / %q", w.GeneratedDescription, w.CommunityDescription, w.Description)
	}
	if g := w.Generated; g == nil || !g.Description || g.Characters || g.Recaps {
		t.Errorf("generated = %+v, want the description alone", g)
	}
	if w.Attribution == nil || len(w.Characters) != 1 || w.Characters[0].ID != "community-hero" {
		t.Errorf("the community's parts changed: %+v", w)
	}
	if after, _ := json.Marshal(env); string(after) != string(snapshot) {
		t.Errorf("the shared envelope changed:\n%s\n%s", snapshot, after)
	}
	raw, _ := json.Marshal(w)
	if !strings.Contains(string(raw), `"generated_description":{"text":"`+testDescription+`"}`) ||
		!strings.Contains(string(raw), `"generated":{"characters":false,"recaps":false,"recap_summary":false,"description":true`) {
		t.Errorf("wire = %s", raw)
	}
	// spoilers=hide keeps it, from the start.
	if hidden := matched(t, env, withDescription(testGenerated()), &Place{}).Work; hidden.GeneratedDescription == nil ||
		hidden.Generated == nil || !hidden.Generated.Description {
		t.Errorf("hidden = %+v / %+v", hidden.GeneratedDescription, hidden.Generated)
	}

	// The community has a description: it wins, nothing generated in its place.
	withCommunity := *full
	withCommunity.CommunityDescription = &MetaCommunityDescription{Text: "Community description."}
	cenv := &Enrichment{Matched: true, Work: &withCommunity}
	if out := matched(t, cenv, withDescription(testGenerated()), nil); out != cenv || out.Work.GeneratedDescription != nil {
		t.Errorf("community description = %+v", out.Work)
	}

	// Unmatched, a description only: matched false with a work of the title, the
	// description, no attribution, kept under spoilers=hide.
	for _, hide := range []*Place{nil, {}} {
		u, ok := Answer(nil, "The Book", withDescription(&Generated{}), hide).(Unmatched)
		if !ok || u.Work == nil || u.Work.Title != "The Book" || u.Work.GeneratedDescription == nil || u.Work.Attribution != nil ||
			u.Work.Generated == nil || !u.Work.Generated.Description || u.Work.Generated.Characters || u.Work.Generated.Recaps {
			t.Errorf("unmatched description only (hide %v) = %+v", hide, u.Work)
		}
	}
	// A work of only a generated description carries no attribution, even
	// matched to a community work with nothing of the CC BY-SA layer.
	bare := &Enrichment{Matched: true, Work: &MetaWork{ID: "w", Title: "W", Description: "Blurb."}}
	if w := matched(t, bare, withDescription(&Generated{}), nil).Work; w.Attribution != nil || w.GeneratedDescription == nil {
		t.Errorf("bare work = %+v", w)
	}
	// A blank description is none.
	blank := &Generated{Description: &model.Description{Text: "  "}}
	if u := Answer(nil, "The Book", blank, nil).(Unmatched); u.Work != nil {
		t.Errorf("blank description = %+v", u.Work)
	}
}

// TestAnswerUnmatched: an unmatched book's work is its generated content alone,
// with the book's title and no attribution; spoiler gating applies to it.
func TestAnswerUnmatched(t *testing.T) {
	t.Parallel()
	unmatched := func(g *Generated, hide *Place) Unmatched {
		t.Helper()
		u, ok := Answer(nil, "The Book", g, hide).(Unmatched)
		if !ok {
			t.Fatalf("an unmatched book answered %T", u)
		}
		return u
	}
	w := unmatched(testGenerated(), nil).Work
	if w == nil || w.ID != "" || w.Title != "The Book" || len(w.Characters) != 2 || len(w.Recaps) != 2 || w.RecapSummary == nil {
		t.Fatalf("work = %+v", w)
	}
	if w.Attribution != nil || w.Generated == nil || !w.Generated.Characters || !w.Generated.Recaps || !w.Generated.RecapSummary {
		t.Errorf("attribution = %+v, generated = %+v", w.Attribution, w.Generated)
	}
	if raw, _ := json.Marshal(Unmatched{Work: w}); !json.Valid(raw) || string(raw[:17]) != `{"matched":false,` {
		t.Errorf("unmatched = %s", raw)
	}
	gated := unmatched(testGenerated(), &Place{Chapter: 2}).Work
	if len(gated.Characters) != 1 || gated.Characters[0].ID != "local-early" || len(gated.Recaps) != 1 || gated.RecapSummary != nil || gated.Attribution != nil {
		t.Errorf("gated = %+v", gated)
	}
	if whole := unmatched(testGenerated(), &Place{Chapter: 2, Finished: true}).Work; whole.RecapSummary == nil || len(whole.Characters) != 2 {
		t.Errorf("a finished book's work was gated: %+v", whole)
	}
	if unmatched(&Generated{}, nil).Work != nil || unmatched(nil, nil).Work != nil {
		t.Error("a work from nothing")
	}
	// Gated to nothing (every character revealed later, every recap past the
	// listener, the summary withheld): plain matched false, no empty work.
	late := &Generated{Characters: &model.Characters{Characters: []model.Character{{ID: "c", Name: "C", Reveal: model.Position{Chapter: 5}}}},
		Recaps: &model.Recaps{Recaps: []model.Recap{{Through: model.Position{Chapter: 4}, Text: "Later."}}, InShort: "Short."}}
	if u := unmatched(late, &Place{Chapter: 1}); u.Work != nil {
		t.Errorf("gated to nothing = %+v, want no work", u.Work)
	}
	if raw, _ := json.Marshal(Answer(nil, "The Book", late, &Place{Chapter: 1})); string(raw) != `{"matched":false}` {
		t.Errorf("gated to nothing = %s", raw)
	}
}

// TestGeneratedRecapSummaryOnTheWire: recap_summary is always sent, false
// included (a client falls back to recaps only when it is absent: an older
// server): false when generated recaps fill a list with no summary at all, or
// beside the community's summary; true when the summary was generated.
func TestGeneratedRecapSummaryOnTheWire(t *testing.T) {
	t.Parallel()
	mark := func(w *MetaWork, g *Generated) string {
		t.Helper()
		b, err := json.Marshal(matched(t, &Enrichment{Matched: true, Work: w}, g, nil).Work.Generated)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	noSummary := testGenerated()
	noSummary.Recaps.InShort, noSummary.Recaps.Ending = "", ""
	at := noSummary.GeneratedAt
	if got, want := mark(communityWork(), noSummary), `{"characters":false,"recaps":true,"recap_summary":false,"description":false,"generated_at":"`+at+`"}`; got != want {
		t.Errorf("no summary: %s, want %s", got, want)
	}
	summed := communityWork()
	summed.RecapSummary = &MetaRecapSummary{InShort: "Community short."}
	if got, want := mark(summed, testGenerated()), `{"characters":false,"recaps":true,"recap_summary":false,"description":false,"generated_at":"`+at+`"}`; got != want {
		t.Errorf("community summary: %s, want %s", got, want)
	}
	if got, want := mark(communityWork(), testGenerated()), `{"characters":false,"recaps":true,"recap_summary":true,"description":false,"generated_at":"`+at+`"}`; got != want {
		t.Errorf("generated summary: %s, want %s", got, want)
	}
}
