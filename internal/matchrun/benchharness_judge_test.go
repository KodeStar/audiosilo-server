//go:build benchharness

package matchrun

import (
	"slices"
	"testing"
)

func TestLFSource(t *testing.T) {
	for _, tc := range []struct {
		trace []string
		want  string
	}{
		{[]string{"LOCAL METADATA:", "SEARCH:", "AUDIBLE MATCH:", "  ASIN:     B09YMW7GS5", "DURATION CHECK:"}, "AUDIBLE"},
		// The last header is the store the pick is from.
		{[]string{"AUDIBLE MATCH:", "  Score: 0.4", "AUDIOBOOK GUILD MATCH:  "}, "AUDIOBOOK GUILD"},
		{[]string{"  Candidate: score=1.0 title=X MATCH:", "Audible match:", "LOCAL METADATA:"}, ""},
		{nil, ""},
	} {
		if got := lfSource(tc.trace); got != tc.want {
			t.Errorf("lfSource(%q) = %q, want %q", tc.trace, got, tc.want)
		}
	}
}

func lfBookOf(cls, asin, isbn string, trace ...string) *lfBook {
	b := &lfBook{Cls: cls, Trace: trace}
	b.Match.ASIN, b.Match.ISBN = asin, isbn
	return b
}

func TestLFPick(t *testing.T) {
	for _, tc := range []struct {
		name            string
		b               *lfBook
		asin, isbn      string
		recordingOfBook bool
	}{
		{"audible pick", lfBookOf("correct", "B0X", "", "AUDIBLE MATCH:"), "B0X", "", false},
		{"held right pick", lfBookOf("correct-held", "B0X", "", "AUDIBLE MATCH:"), "B0X", "", false},
		{"another recording", lfBookOf("recording", "B0X", "", "AUDIBLE MATCH:"), "B0X", "", true},
		// Another store's id is no ASIN; its ISBN still names the book.
		{"guild id", lfBookOf("correct", "RGP-17", "", "AUDIBLE MATCH:", "AUDIOBOOK GUILD MATCH:"), "", "", false},
		{"store isbn", lfBookOf("partial", "", "9781427262240", "GRAPHICAUDIO MATCH:"), "", "9781427262240", false},
		// A pick LibraForge got wrong, or none, names nothing.
		{"wrong", lfBookOf("wrong", "B0X", "", "AUDIBLE MATCH:"), "", "", false},
		{"none", lfBookOf("none", "B0X", "", "AUDIBLE MATCH:"), "", "", false},
	} {
		asin, isbn, rec := lfPick(tc.b)
		if asin != tc.asin || isbn != tc.isbn || rec != tc.recordingOfBook {
			t.Errorf("%s: lfPick = %q, %q, %v, want %q, %q, %v", tc.name, asin, isbn, rec, tc.asin, tc.isbn, tc.recordingOfBook)
		}
	}
}

func TestHandVerdict(t *testing.T) {
	for _, tc := range []struct {
		o         []string
		right, ok bool
	}{
		{[]string{"correct", "B0X"}, true, true},
		{[]string{"partial", "B0X"}, true, true},
		{[]string{"correct-held", "B0X"}, true, true},
		{[]string{"wrong-held", "B0X"}, false, true},
		{[]string{"wrong", "B0X"}, false, true},
		{[]string{"none", "B0X"}, false, false},
		{[]string{"not run", ""}, false, false},
		{[]string{"correct", ""}, false, false},
		{nil, false, false},
	} {
		b := &lfBook{O: map[string][]string{"audiosilo": tc.o}}
		_, class, ok := handVerdict(b)
		if right := slices.Contains(rightBook, class); right != tc.right || ok != tc.ok {
			t.Errorf("handVerdict(%q) = %v, %v, want %v, %v", tc.o, right, ok, tc.right, tc.ok)
		}
	}
}

func TestVerdictOf(t *testing.T) {
	for _, tc := range []struct {
		name            string
		top             string
		a               answers
		right, unjudged bool
		by              string
	}{
		{"ref ASIN right", "w1", answers{refWork: "w1", refHasASIN: true, hasTitle: true}, true, false, byRefASIN},
		{"ref ASIN wrong, title alike", "w2", answers{refWork: "w1", refHasASIN: true, hasTitle: true, titleSame: true}, false, false, byRefASIN},
		// A reference ASIN this copy doesn't know: no other pick is read.
		{"unknown ref ASIN", "w2", answers{refHasASIN: true, lfWork: "w2", handWork: "w2", handRight: true, hasTitle: true}, false, false, byTitle},
		{"lf pick right, title differs", "w1", answers{lfWork: "w1", hasTitle: true}, true, false, byLFPick},
		// LibraForge's pick comes before the audit of the same work.
		{"lf pick wrong, hand right", "w2", answers{lfWork: "w1", handWork: "w2", handRight: true, hasTitle: true, titleSame: true}, false, false, byLFPick},
		{"hand right, title differs", "w2", answers{handWork: "w2", handRight: true, hasTitle: true}, true, false, byHand},
		{"hand wrong, title alike", "w2", answers{handWork: "w2", hasTitle: true, titleSame: true}, false, false, byHand},
		{"hand on another work: title", "w3", answers{handWork: "w2", handRight: true, hasTitle: true, titleSame: true}, true, false, byTitle},
		{"no pick: title", "w2", answers{hasTitle: true, titleSame: true}, true, false, byTitle},
		{"no pick, no title", "w2", answers{}, false, true, ""},
	} {
		right, unjudged, by := verdictOf(tc.top, tc.a)
		if right != tc.right || unjudged != tc.unjudged || by != tc.by {
			t.Errorf("%s: verdictOf = %v, %v, %q, want %v, %v, %q", tc.name, right, unjudged, by, tc.right, tc.unjudged, tc.by)
		}
	}
}

func TestTallyAndAgreement(t *testing.T) {
	rs := []lfResult{
		{Outcome: "auto", Work: "w1", Right: true, RecordingRight: true, RefHasASIN: true, HandWork: "w1", HandRight: true},
		{Outcome: "auto", Work: "w2", Right: true, TitleRight: false, HandWork: "w2", HandRight: true},
		{Outcome: "review", Work: "w3", Right: false, TitleRight: true, HandWork: "w3", HandRight: false},
		{Outcome: "review", Work: "w4", Unjudged: true, TitleUnjudged: true, HandWork: "w4"},
		{Outcome: "none", Detail: DetailWeakMatch, Work: "w5", Right: true, HandWork: "w9", HandRight: true},
		{Outcome: "none"},
	}
	all := func(*lfResult) bool { return true }
	noASIN := func(r *lfResult) bool { return !r.RefHasASIN }

	got := tally(rs, all, newJudging)
	want := map[string]int{"auto-right": 2, "auto-right, recording right": 1, "review-wrong": 1, "review-unjudged": 1,
		"none": 2, "none: weak_match": 1, "none: weak_match, top was right": 1}
	for _, k := range tableKeys {
		if got[k] != want[k] {
			t.Errorf("tally new [%s] = %d, want %d", k, got[k], want[k])
		}
	}
	got = tally(rs, noASIN, titleJudging)
	if got["auto-wrong"] != 1 || got["review-right"] != 1 || got["auto-right"] != 0 {
		t.Errorf("tally title, no ASIN = %v", got)
	}

	// Shared picks: w1, w2, w3 (w4 unjudged, w5's hand pick is another work,
	// w6 judged by the hand verdict itself).
	rs = append(rs, lfResult{Outcome: "auto", Work: "w6", JudgedBy: byHand, Right: true, HandWork: "w6"})
	if agree, shared := agreement(rs, all, newJudging); agree != 3 || shared != 3 {
		t.Errorf("agreement new = %d of %d, want 3 of 3", agree, shared)
	}
	if agree, shared := agreement(rs, noASIN, titleJudging); agree != 0 || shared != 2 {
		t.Errorf("agreement title, no ASIN = %d of %d, want 0 of 2", agree, shared)
	}
}
