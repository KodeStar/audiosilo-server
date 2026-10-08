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

// QueryFor is the match query for book b on a server preferring region: its own
// facts, as both the match dialog and a bulk run send them.
func QueryFor(b *catalog.Book, region string) meta.MatchQuery {
	return meta.MatchQuery{
		Title: b.Title, Series: b.Series, SeriesIndex: b.SeriesIndex, Author: b.Author, Duration: b.Duration,
		Path: b.RelPath, IsFolder: b.IsFolder, BookASIN: b.ASIN, BookISBN: b.ISBN, Region: region,
	}
}

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
		// The work's other series (Plan decides which is the book's main one). A
		// position that is no plain number (a range, "1-3") is no position.
		more := make([]catalog.SeriesRef, 0, len(c.Series)-1)
		for _, s := range c.Series[1:] {
			pos := catalog.ParseSeriesIndex(catalog.CleanOverride(catalog.FieldSeriesIndex, s.Position))
			more = append(more, catalog.SeriesRef{Name: s.Name, Position: pos})
		}
		raw[catalog.FieldMoreSeries] = catalog.EncodeMoreSeries(more)
	}
	if rec != nil {
		p.RecordingID, p.Narrators, p.RuntimeMin = rec.ID, names(rec.Narrators), rec.RuntimeMin
		raw[catalog.FieldNarrator] = p.Narrators
		if published == "" {
			published = rec.ReleaseDate
		}
		if len(rec.ASINs) > 0 {
			raw[catalog.FieldASIN], p.ASINRegion = rec.ASINs[0], rec.ASINRegion
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
		switch field {
		case catalog.FieldSeries, catalog.FieldSeriesIndex, catalog.FieldMoreSeries:
			// planSeries: which of the work's series is offered as the main one
			// depends on the book's own.
		default:
			plan(scope, field, p.Values[field], st.Fields[field], set)
		}
	}
	planSeries(scope, st, p, set)
	cover = scope != ScopeIDs && st.CoverMissing && p.CoverURL != ""
	return set, cover
}

// plan puts the community's value for field (theirs) in set when the scope takes
// it over the book's (mine): every scope fills a missing identifier, fill fills
// any missing field, overwrite replaces too. Never an admin's own edit, nor a
// value the book already has.
func plan(scope, field, theirs string, mine catalog.FieldValue, set map[string]string) {
	if theirs == "" || mine.Source == catalog.SourceEdited || catalog.CleanOverride(field, mine.Value) == theirs {
		return
	}
	empty := strings.TrimSpace(mine.Value) == ""
	identifier := field == catalog.FieldASIN || field == catalog.FieldISBN
	if scope == ScopeOverwrite || (empty && (scope == ScopeFill || identifier)) {
		set[field] = theirs
	}
}

// planSeries plans the series, the position and the other series. The community
// lists every series of the work (its main one first, then p.Values'
// more_series) and offers one as the book's main series: the book's own when the
// work is in it, else the work's first (as the match dialog's communityValues
// does), so even overwrite keeps a book filed under another of the work's series.
// The book keeps one main series - its own, or the one being set - so its
// position is the community's position in THAT series (its first listing; none
// when the community doesn't list it), and the work's other series become its
// more_series. A main series the community doesn't list at all is most likely
// the work's main series spelled the book's own way ("Stormlight Archive" for
// "The Stormlight Archive"), so that one isn't added beside it as another.
func planSeries(scope string, st *catalog.MatchState, p catalog.MatchProposal, set map[string]string) {
	var work []catalog.SeriesRef
	if name := p.Values[catalog.FieldSeries]; name != "" {
		work = append(work, catalog.SeriesRef{Name: name, Position: catalog.ParseSeriesIndex(p.Values[catalog.FieldSeriesIndex])})
	}
	work = append(work, catalog.ParseMoreSeries(p.Values[catalog.FieldMoreSeries])...)
	own := st.Fields[catalog.FieldSeries].Value
	if len(work) > 0 {
		offered := max(slices.IndexFunc(work, func(s catalog.SeriesRef) bool { return sameSeries(s.Name, own) }), 0)
		plan(scope, catalog.FieldSeries, work[offered].Name, st.Fields[catalog.FieldSeries], set)
	}
	main := own
	if v, ok := set[catalog.FieldSeries]; ok {
		main = v
	}
	mainPos, found := "", false
	unlisted := main != "" && !slices.ContainsFunc(work, func(s catalog.SeriesRef) bool { return sameSeries(s.Name, main) })
	others := []catalog.SeriesRef{}
	for i, s := range work {
		if !sameSeries(s.Name, main) {
			if !(unlisted && i == 0) {
				others = append(others, s)
			}
		} else if !found {
			// A work listed twice in its main series keeps its first position.
			mainPos, found = catalog.FormatSeriesPosition(s.Position), true
		}
	}
	plan(scope, catalog.FieldSeriesIndex, mainPos, st.Fields[catalog.FieldSeriesIndex], set)
	// In stored form (each name once, within the bounds), as the book's own value
	// is compared and as an apply would store it.
	plan(scope, catalog.FieldMoreSeries, catalog.CleanOverride(catalog.FieldMoreSeries, catalog.EncodeMoreSeries(others)),
		st.Fields[catalog.FieldMoreSeries], set)
}

// sameSeries reports whether two series names name one series (match.SeriesKey,
// as the console's seriesKey compares them).
func sameSeries(a, b string) bool {
	return b != "" && match.SeriesKey(a) == match.SeriesKey(b)
}

// planFor is what applying p to a book in state st writes for a run in mode: a
// repick's ASIN (whatever the scope), else Plan's.
func planFor(mode, scope string, st *catalog.MatchState, p catalog.MatchProposal) (map[string]string, bool) {
	if mode == catalog.MatchModeRepick {
		return PlanRepick(st, p), false
	}
	return Plan(scope, st, p)
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
