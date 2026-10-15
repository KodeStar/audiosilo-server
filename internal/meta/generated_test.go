package meta

import (
	"encoding/json"
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

// TestWithGeneratedFillsWhatTheCommunityLacks: community wins per dimension;
// generated recaps (and their summary) fill a work without any; the cached
// envelope is never written to.
func TestWithGeneratedFillsWhatTheCommunityLacks(t *testing.T) {
	t.Parallel()
	env := &Enrichment{Matched: true, Work: communityWork()}
	snapshot, _ := json.Marshal(env)
	out := WithGenerated(env, testGenerated())
	w := out.Work
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

	// Gated to nothing community, the credit goes; the generated recap at chapter
	// 0 stays marked.
	gated := HideSpoilers(&Enrichment{Work: &MetaWork{ID: "w", Characters: []MetaCharacter{{ID: "c", Reveal: MetaPosition{Chapter: 9}}},
		Attribution: &MetaAttribution{}}}, 0, false)
	if gated.Work.Attribution != nil {
		t.Errorf("gated community-free work kept attribution")
	}
	hidden := HideSpoilers(out, 0, false).Work
	if hidden.Attribution == nil || hidden.Generated == nil || !hidden.Generated.Recaps || len(hidden.Recaps) != 1 || hidden.RecapSummary != nil {
		t.Errorf("hidden = %+v / %+v", hidden, hidden.Generated)
	}

	// A work with both from the community keeps them: nothing generated.
	both := communityWork()
	both.Recaps = []MetaRecap{{Through: MetaPosition{Chapter: 1}, Text: "Community."}}
	env = &Enrichment{Matched: true, Work: both}
	if out := WithGenerated(env, testGenerated()); out != env || out.Work.Generated != nil {
		t.Errorf("both community = %+v", out.Work)
	}

	// A community summary stays beside generated recaps, and still counts as
	// community content.
	summed := &MetaWork{ID: "w", RecapSummary: &MetaRecapSummary{InShort: "Community short."}, Attribution: &MetaAttribution{}}
	out = WithGenerated(&Enrichment{Work: summed}, testGenerated())
	if out.Work.RecapSummary.InShort != "Community short." || out.Work.Attribution == nil || len(out.Work.Recaps) != 2 {
		t.Errorf("community summary = %+v", out.Work)
	}
}

// TestGeneratedWork: an unmatched book's work is its generated content alone,
// with the book's title and no attribution; spoiler gating applies to it.
func TestGeneratedWork(t *testing.T) {
	t.Parallel()
	w := GeneratedWork("The Book", testGenerated())
	if w == nil || w.ID != "" || w.Title != "The Book" || len(w.Characters) != 2 || len(w.Recaps) != 2 || w.RecapSummary == nil {
		t.Fatalf("work = %+v", w)
	}
	if w.Attribution != nil || w.Generated == nil || !w.Generated.Characters || !w.Generated.Recaps {
		t.Errorf("attribution = %+v, generated = %+v", w.Attribution, w.Generated)
	}
	if raw, _ := json.Marshal(Unmatched{Work: w}); !json.Valid(raw) || string(raw[:17]) != `{"matched":false,` {
		t.Errorf("unmatched = %s", raw)
	}
	gated := GateWork(w, 2, false)
	if len(gated.Characters) != 1 || gated.Characters[0].ID != "local-early" || len(gated.Recaps) != 1 || gated.RecapSummary != nil || gated.Attribution != nil {
		t.Errorf("gated = %+v", gated)
	}
	if GateWork(w, 2, true) != w {
		t.Error("a finished book's work was gated")
	}
	if GeneratedWork("T", &Generated{}) != nil || GeneratedWork("T", nil) != nil {
		t.Error("a work from nothing")
	}
}
