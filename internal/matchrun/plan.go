// Package matchrun matches many books against the community metadata at once
// (Health > Not matched > Find matches). A run first only looks: it matches each
// book the way the console's match dialog does (meta.Service.Candidates), sorts it
// confident / needs review / no match, and records what the best candidate offers.
// Nothing changes until the admin applies the run, choosing how much it may write
// (a Scope). Applying writes community overrides (catalog.EditBook, source
// community) and, where a book has no cover, the community cover, so every change
// is a reversible override, exactly as accepting a match by hand is.
//
// A repick run looks again at books whose ASIN a community match set, for the same
// recording's ASIN in the preferred Audible marketplace (config metadata.region),
// which the community data may have gained since.
package matchrun

import (
	"regexp"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// What a match must have to be applied unasked: a score of at least AutoScore,
// AutoLead ahead of the next candidate (two works that both fit are a choice for
// the admin), and an ASIN or ISBN to attach (the book counts as matched by it).
const (
	AutoScore = 90
	AutoLead  = 10
)

// How much applying a run may write.
const (
	// ScopeIDs attaches the ASIN/ISBN only: the book is matched, and the player's
	// community enrichment finds it; everything the files say stays.
	ScopeIDs = "ids"
	// ScopeFill also fills every field the book has empty, and its cover when it
	// has none; nothing it has is replaced.
	ScopeFill = "fill"
	// ScopeOverwrite takes every community value that differs, as accepting the
	// match dialog with its default ticks does. An admin's own edit stays either way.
	ScopeOverwrite = "overwrite"
)

// Scopes lists the scopes, narrowest first.
var Scopes = []string{ScopeIDs, ScopeFill, ScopeOverwrite}

// ValidScope reports whether s is one of Scopes.
func ValidScope(s string) bool { return slices.Contains(Scopes, s) }

var yearRE = regexp.MustCompile(`^\d{4}`)

// Propose is what candidate c offers a book as recording rec (nil = none): the
// match dialog's community values (admin-ui match-model.ts communityValues), each
// in stored form, a value an override would refuse left out. The ASIN is the
// recording's best for this server (meta.MatchRecording.ASINs is ordered by the
// preferred marketplace), named with its marketplace.
func Propose(c *meta.MatchCandidate, rec *meta.MatchRecording) catalog.MatchProposal {
	p := catalog.MatchProposal{
		WorkID: c.WorkID, Title: c.Title, Authors: names(c.Authors), WebURL: c.WebURL,
		CoverURL: c.CoverURL, Values: map[string]string{},
	}
	published := c.FirstPublished
	raw := map[string]string{
		catalog.FieldTitle: c.Title, catalog.FieldAuthor: p.Authors, catalog.FieldDescription: c.Description,
	}
	if len(c.Series) > 0 {
		raw[catalog.FieldSeries], raw[catalog.FieldSeriesIndex] = c.Series[0].Name, c.Series[0].Position
	}
	if rec != nil {
		p.RecordingID, p.Narrators, p.RuntimeMin = rec.ID, names(rec.Narrators), rec.RuntimeMin
		raw[catalog.FieldNarrator] = p.Narrators
		if published == "" {
			published = rec.ReleaseDate
		}
		if len(rec.ASINs) > 0 {
			raw[catalog.FieldASIN] = rec.ASINs[0]
			p.ASINRegion = regionOf(rec, rec.ASINs[0])
		}
		if len(rec.ISBNs) > 0 {
			raw[catalog.FieldISBN] = rec.ISBNs[0]
		}
		if rec.CoverURL != "" {
			p.CoverURL = rec.CoverURL // the recording's cover, else the work's
		}
	}
	raw[catalog.FieldPublished] = yearRE.FindString(published)
	for field, v := range raw {
		if v = catalog.CleanOverride(field, v); v != "" {
			p.Values[field] = v
		}
	}
	if p.Values[catalog.FieldASIN] == "" {
		p.ASINRegion = ""
	}
	return p
}

// regionOf is the marketplace an ASIN of rec sells in: the first metaserve lists
// it under, as the ASINs are ordered (the preferred region's comes first).
func regionOf(rec *meta.MatchRecording, asin string) string {
	for _, r := range rec.ASINRefs {
		if r.ASIN == asin {
			return r.Region
		}
	}
	return ""
}

func names(people []meta.MetaPersonRef) string {
	out := make([]string, 0, len(people))
	for _, p := range people {
		out = append(out, p.Name)
	}
	return strings.Join(out, ", ")
}

// Plan is what applying proposal p to a book in state st writes under scope: the
// fields to set (as community overrides) and whether to take the community cover.
// It is the one statement of the scopes, so what the review shows and what an
// apply writes can't disagree. Never: a value the book already has, an admin's own
// edit, a series position for a series the book won't have, or a cover over any
// the book has.
func Plan(scope string, st *catalog.MatchState, p catalog.MatchProposal) (set map[string]string, cover bool) {
	set = map[string]string{}
	for _, field := range catalog.OverrideFields {
		theirs := p.Values[field]
		mine := st.Fields[field]
		if theirs == "" || mine.Source == catalog.SourceEdited || catalog.CleanOverride(field, mine.Value) == theirs {
			continue
		}
		empty := strings.TrimSpace(mine.Value) == ""
		identifier := field == catalog.FieldASIN || field == catalog.FieldISBN
		if scope == ScopeOverwrite || (empty && (scope == ScopeFill || identifier)) {
			set[field] = theirs
		}
	}
	// A position numbers its series: one goes on only beside the series it
	// belongs to, the book's own or the one being set.
	if _, ok := set[catalog.FieldSeriesIndex]; ok {
		series := st.Fields[catalog.FieldSeries].Value
		if v, ok := set[catalog.FieldSeries]; ok {
			series = v
		}
		if match.Fold(series) != match.Fold(p.Values[catalog.FieldSeries]) {
			delete(set, catalog.FieldSeriesIndex)
		}
	}
	cover = scope != ScopeIDs && st.CoverMissing && p.CoverURL != ""
	return set, cover
}

// PlanRepick is what applying a repick item writes: its ASIN, while the book's
// ASIN is still the community override the run found (an admin who since edited
// it, or reverted it to the tags, keeps theirs).
func PlanRepick(st *catalog.MatchState, p catalog.MatchProposal) map[string]string {
	cur := st.Fields[catalog.FieldASIN]
	asin := p.Values[catalog.FieldASIN]
	if asin == "" || !cur.Locked || cur.Source != catalog.SourceCommunity || cur.Value == asin {
		return map[string]string{}
	}
	return map[string]string{catalog.FieldASIN: asin}
}

// Classify sorts a book's candidates (best first) into an outcome: confident
// (OutcomeAuto), worth a look (OutcomeReview) or none, with the best score and the
// next one's.
func Classify(cands []meta.MatchCandidate, p catalog.MatchProposal) (outcome string, score, runnerUp int) {
	if len(cands) == 0 {
		return catalog.OutcomeNone, 0, 0
	}
	score = cands[0].Score
	if len(cands) > 1 {
		runnerUp = cands[1].Score
	}
	identified := p.Values[catalog.FieldASIN] != "" || p.Values[catalog.FieldISBN] != ""
	if score >= AutoScore && score-runnerUp >= AutoLead && identified {
		return catalog.OutcomeAuto, score, runnerUp
	}
	return catalog.OutcomeReview, score, runnerUp
}
