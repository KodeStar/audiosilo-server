package meta

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"
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
	if g := w.Generated; g == nil || g.Characters || !g.Recaps || g.GeneratedAt != "2026-10-15T10:00:00Z" {
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
	if hidden.Attribution == nil || hidden.Generated == nil || !hidden.Generated.Recaps || len(hidden.Recaps) != 1 || hidden.RecapSummary != nil {
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
	if env.Fillable() {
		t.Error("a work with characters, recaps and a description is fillable")
	}
	// Without the community description there is one thing to fill.
	noDesc := *both
	noDesc.CommunityDescription = nil
	if !(&Enrichment{Work: &noDesc}).Fillable() {
		t.Error("a work without a community description can't be filled")
	}
	if !(&Enrichment{Work: &MetaWork{Characters: both.Characters}}).Fillable() || !(*Enrichment)(nil).Fillable() {
		t.Error("a work without recaps, or no match, can't be filled")
	}

	// A community summary stays beside generated recaps, and still counts as
	// community content.
	summed := &MetaWork{ID: "w", RecapSummary: &MetaRecapSummary{InShort: "Community short."}, Attribution: &MetaAttribution{}}
	out := matched(t, &Enrichment{Work: summed}, testGenerated(), nil)
	if out.Work.RecapSummary.InShort != "Community short." || out.Work.Attribution == nil || len(out.Work.Recaps) != 2 {
		t.Errorf("community summary = %+v", out.Work)
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
		!strings.Contains(string(raw), `"generated":{"characters":false,"recaps":false,"description":true`) {
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
	if w.Attribution != nil || w.Generated == nil || !w.Generated.Characters || !w.Generated.Recaps {
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
