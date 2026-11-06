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
// which the community data may have gained since. A refresh run looks the books
// that already have an ASIN or ISBN up by it alone, to fill in the details an
// identifier alone didn't bring; it never changes the identifier.
package matchrun

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/names"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// What a match must have to be applied unasked: a score of at least AutoScore,
// AutoLead ahead of the next candidate (two works that both fit are a choice for
// the admin), an ASIN or ISBN to attach (the book counts as matched by it), and,
// when both lengths are known, a recording within meta.PlausibleRuntimeOff of the book's
// length (the closest recording of a work can still be another edition: the
// unabridged book of a work that holds only its dramatization).
// An identifier hit (the ASIN or ISBN the query sent named the work: in a run
// always a tag hint, a book with its own being no run's subject) needs no lead:
// it is the book. A runner-up that is one too would be two works holding one
// identifier, the admin's choice; metaserve names one work per request today, so
// that is a guard, not a case a run meets.
const (
	AutoScore = 90
	AutoLead  = 10
)

// ReviewFloor is the metaserve score a match run's best candidate needs to be
// held for review: below it the community most likely doesn't have the book (a
// weak fit is almost always another book), so the item records no match, detail
// DetailWeakMatch. An older metaserve's fallback search scores on another scale
// (scoreCandidate), where the floor says nothing: such a candidate stays for
// review. The match dialog has no floor: it lists up to meta's
// maxMatchCandidates (6) whatever their scores.
const ReviewFloor = 60

// How much applying a run may write.
const (
	// ScopeIDs attaches the ASIN/ISBN only: the book is matched, and the player's
	// community enrichment finds it; everything the files say stays.
	ScopeIDs = "ids"
	// ScopeFill also fills every field the book has empty, and its cover when it
	// has none; nothing it has is replaced, except a cluttered title it tidies
	// (tidyTitle).
	ScopeFill = "fill"
	// ScopeOverwrite takes every community value that differs, as accepting the
	// match dialog with its default ticks does. An admin's own edit stays either way.
	ScopeOverwrite = "overwrite"
)

// Scopes lists the scopes, narrowest first.
var Scopes = []string{ScopeIDs, ScopeFill, ScopeOverwrite}

// ValidScope reports whether s is one of Scopes.
func ValidScope(s string) bool { return slices.Contains(Scopes, s) }

// ScopesFor is the scopes an apply of a run in mode may name: every scope for a
// match; fill and overwrite for a refresh (it never writes an identifier, so ids
// would write nothing); none for a repick, which writes its ASIN whatever the
// scope says.
func ScopesFor(mode string) []string {
	switch mode {
	case catalog.MatchModeRepick:
		return nil
	case catalog.MatchModeRefresh:
		return []string{ScopeFill, ScopeOverwrite}
	}
	return Scopes
}

// QueryFor is the match query for book b on a server preferring region: its own
// facts, as both the match dialog and a bulk run send them. A book with no ASIN
// or ISBN of its own sends its tags' (Book.TagASIN, TagISBN) as hints: metaserve
// looks them up like the book's own, and an identifier hit whose length fits is
// confident (Classify), but the book is matched only by what an apply writes.
// Its language is the book's, else its tags' (meta.NormalizeLanguage), else its
// library's (libraryLanguage, LibraryLanguage's answer: "" for none): never the
// region's, as a marketplace doesn't say what language a library is in.
// Its narrators are its narrator credit's names, its genres the book's.
func QueryFor(b *catalog.Book, region, libraryLanguage string) meta.MatchQuery {
	asin, isbn := b.ASIN, b.ISBN
	if asin == "" && isbn == "" {
		asin, isbn = b.TagASIN, b.TagISBN
	}
	return meta.MatchQuery{
		Title: b.Title, Series: b.Series, SeriesIndex: b.SeriesIndex, SeriesIndexEnd: b.SeriesIndexEnd,
		Author: b.Author, Duration: b.Duration,
		Path: b.RelPath, IsFolder: b.IsFolder, BookASIN: asin, BookISBN: isbn, Region: region,
		Language:  cmp.Or(bookLanguage(b.Language, b.TagLanguage), libraryLanguage),
		Narrators: names.Split(b.Narrator), Genres: b.Genres,
	}
}

// bookLanguage is the language a book says it is in: its own, else its tags'.
func bookLanguage(language, tagLanguage string) string {
	return cmp.Or(meta.NormalizeLanguage(language), meta.NormalizeLanguage(tagLanguage))
}

// What a library's books must say for a match to take their language as an untagged
// book's (LibraryLanguage). A wrong guess scores the book's own work as another
// language's, the very edition mix-up the language is sent to prevent, so the library
// must all but speak one: LanguageMinShare of the books that say a language, and
// LanguageMinBooks of them at least, as a handful says nothing of a library.
// The benchmark library is the shape this is for: 1,593 of 4,044 books tagged, every
// one English. A library of two languages, 80/20, takes none: a fifth of its
// untagged books would be sent the wrong one.
const (
	LanguageMinBooks = 20
	LanguageMinShare = 0.9
)

// LibraryLanguage is the language a library's books (counts, catalog.LanguageCounts)
// all but agree on (LanguageMinShare of those saying one, at least LanguageMinBooks of
// them), else "": what a book saying none is matched in (QueryFor). A book's own
// language, else its tags' (bookLanguage), is what it says, its spellings read as one
// language (languageCode: "English", "eng" and "en" are "en", the code sent).
func LibraryLanguage(counts []catalog.LanguageCount) string {
	by, said := map[string]int{}, 0
	for _, n := range counts {
		if l := languageCode(bookLanguage(n.Language, n.TagLanguage)); l != "" {
			by[l] += n.Books
			said += n.Books
		}
	}
	for l, n := range by {
		if said >= LanguageMinBooks && float64(n) >= LanguageMinShare*float64(said) {
			return l
		}
	}
	return ""
}

// languageWords are the language names and ISO 639-2 codes a tag spells a language
// with, beyond the English names (model.LanguageWords), each as its 639-1 code: the
// commonest of the table metaserve reads them by (audiosilo-meta pkg/query
// matchLanguageWords).
var languageWords = func() map[string]string {
	m := model.LanguageWords()
	maps.Copy(m, map[string]string{
		"eng": "en", "ger": "de", "deu": "de", "fre": "fr", "fra": "fr", "spa": "es", "ita": "it",
		"jpn": "ja", "por": "pt", "dut": "nl", "nld": "nl", "pol": "pl", "rus": "ru", "chi": "zh",
		"zho": "zh", "dan": "da", "swe": "sv", "fin": "fi", "nor": "no", "nob": "no", "nno": "no",
		"nb": "no", "nn": "no", "tur": "tr", "cze": "cs", "ces": "cs", "hun": "hu",
		"deutsch": "de", "français": "fr", "francais": "fr", "español": "es", "espanol": "es",
		"castellano": "es", "italiano": "it", "nederlands": "nl", "português": "pt", "polski": "pl",
		"svenska": "sv", "dansk": "da", "norsk": "no", "suomi": "fi",
		"portugues": "pt", //nolint:misspell // Portuguese without its accent, as a tag spells it
	})
	return m
}()

// languageCode is a language as bookLanguage reads it ("english", "eng", "de"), as its
// 639-1 code when languageWords knows the spelling, else as it is.
func languageCode(l string) string { return cmp.Or(languageWords[l], l) }

var yearRE = regexp.MustCompile(`^\d{4}`)

// Propose is what candidate c offers a book as recording rec (nil = none): the
// match dialog's community values (admin-ui match-model.ts communityValues), each
// in stored form, a value an override would refuse left out. The ASIN is the
// recording's best for this server (meta.MatchRecording.ASINs is ordered by the
// preferred marketplace), named with its marketplace. The series are the ones a
// book that is rec is in (meta.MatchRecording.Series): an edition named for that
// recording first, never an edition of another recording. The subtitle and
// language are the work's, the publisher the recording's, and abridged the
// recording's claim (catalog.AbridgedClaim). The community's genres are not
// offered (a match candidate carries none).
func Propose(c *meta.MatchCandidate, rec *meta.MatchRecording) catalog.MatchProposal {
	p := catalog.MatchProposal{
		WorkID: c.WorkID, Title: c.Title, Authors: joinNames(c.Authors), WebURL: c.WebURL,
		CoverURL: c.CoverURL, Values: map[string]string{},
	}
	published := c.FirstPublished
	raw := map[string]string{
		catalog.FieldTitle: c.Title, catalog.FieldSubtitle: c.Subtitle, catalog.FieldAuthor: p.Authors,
		catalog.FieldLanguage: c.Language, catalog.FieldDescription: c.Description,
	}
	series := c.Series
	if rec != nil && rec.Series != nil {
		series = rec.Series
	}
	if len(series) > 0 {
		raw[catalog.FieldSeries], raw[catalog.FieldSeriesIndex] = series[0].Name, series[0].Position
		// The work's other series (Plan decides which is the book's main one). A
		// position the series_index field would refuse is no position; a range
		// ("1-3", an omnibus) is kept as one.
		more := make([]catalog.SeriesRef, 0, len(series)-1)
		for _, s := range series[1:] {
			more = append(more, catalog.SeriesAt(s.Name, catalog.CleanOverride(catalog.FieldSeriesIndex, s.Position)))
		}
		raw[catalog.FieldMoreSeries] = catalog.EncodeMoreSeries(more)
	}
	if rec != nil {
		p.RecordingID, p.Narrators, p.RuntimeMin = rec.ID, joinNames(rec.Narrators), rec.RuntimeMin
		raw[catalog.FieldNarrator], raw[catalog.FieldPublisher] = p.Narrators, rec.Publisher
		raw[catalog.FieldAbridged] = catalog.AbridgedClaim(rec.Abridged)
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

func joinNames(people []meta.MetaPersonRef) string {
	out := make([]string, 0, len(people))
	for _, p := range people {
		out = append(out, p.Name)
	}
	return strings.Join(out, ", ")
}

// Writes is what applying a match writes to a book: the fields to Set (as
// community overrides), the community overrides to Revert (a series the community
// wrote earlier and no longer lists for the book) and whether to take the
// community Cover.
type Writes struct {
	Set    map[string]string
	Revert []string
	Cover  bool
}

// Empty reports whether w writes nothing.
func (w Writes) Empty() bool { return len(w.Set) == 0 && len(w.Revert) == 0 && !w.Cover }

// after is field's value on a book in state st once w is applied: the value it
// sets, else what the files say for one it reverts, else the value it has.
func (w Writes) after(st *catalog.MatchState, field string) string {
	if v, ok := w.Set[field]; ok {
		return v
	}
	if slices.Contains(w.Revert, field) {
		return st.Fields[field].Scanned
	}
	return st.Fields[field].Value
}

// Plan is what applying proposal p to a book in state st writes under scope. It is
// the one statement of the scopes, so what the review shows and what an apply
// writes can't disagree. Fill fills empty fields, and replaces a title only to
// tidy it (tidyTitle). Never: a value the book already has, an admin's own edit,
// a series position for a series the book won't have, or a cover over any the
// book has.
func Plan(scope string, st *catalog.MatchState, p catalog.MatchProposal) Writes {
	w := Writes{Set: map[string]string{}}
	for _, field := range catalog.OverrideFields {
		switch field {
		case catalog.FieldSeries, catalog.FieldSeriesIndex, catalog.FieldMoreSeries:
			// planSeries: which of the work's series is offered as the main one
			// depends on the book's own.
		default:
			plan(scope, field, p.Values[field], st.Fields[field], w.Set)
		}
	}
	planSeries(scope, st, p, &w)
	// Fill also tidies a title the files clutter (tidyTitle): the community's
	// title is the book's, less the series and "Book 2 (Unabridged)".
	if scope == ScopeFill {
		if t := tidyTitle(st, p); t != "" {
			w.Set[catalog.FieldTitle] = t
		}
	}
	w.Cover = scope != ScopeIDs && st.CoverMissing && p.CoverURL != ""
	return w
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
// lists the series a book of the work is in (its main one first, then p.Values'
// more_series: meta.SeriesFor's rules) and offers one as the book's main series:
// the book's own when the work is in it, else the work's first (as the match
// dialog's communityValues does), so even overwrite keeps a book filed under
// another of the work's series. The book keeps one main series - its own, or the
// one being set - so its position is the community's position in THAT series
// (its first listing; none when the community doesn't list it), and the work's
// other series become its more_series. A main series the community doesn't list
// at all is most likely the work's main series spelled the book's own way
// ("Stormlight Archive" for "The Stormlight Archive"), so that one isn't added
// beside it as another.
//
// Overwrite REPLACES the book's series with the community's, it never adds to
// them: a series value the community itself wrote earlier (source community)
// that the community no longer offers - a series it deleted or merged, an
// edition or a translation the series rules now leave out - is reverted, back to
// what the files say. Only the series fields: a community title or narrator the
// candidate lacks says nothing (a recording not picked), while a series it lacks
// is the series rules' answer. An admin's own series stay under every scope.
func planSeries(scope string, st *catalog.MatchState, p catalog.MatchProposal, w *Writes) {
	set := w.Set
	stale := func(field string) {
		if mine := st.Fields[field]; scope == ScopeOverwrite && mine.Source == catalog.SourceCommunity && mine.Value != "" {
			w.Revert = append(w.Revert, field)
		}
	}
	var work []catalog.SeriesRef
	if name := p.Values[catalog.FieldSeries]; name != "" {
		work = append(work, catalog.SeriesAt(name, p.Values[catalog.FieldSeriesIndex]))
	}
	work = append(work, catalog.ParseMoreSeries(p.Values[catalog.FieldMoreSeries])...)
	own := st.Fields[catalog.FieldSeries].Value
	if len(work) > 0 {
		offered := max(slices.IndexFunc(work, func(s catalog.SeriesRef) bool { return match.SameSeries(s.Name, own) }), 0)
		plan(scope, catalog.FieldSeries, work[offered].Name, st.Fields[catalog.FieldSeries], set)
	} else {
		stale(catalog.FieldSeries)
	}
	main := w.after(st, catalog.FieldSeries)
	mainPos, found := "", false
	unlisted := len(work) > 0 && main != "" && !slices.ContainsFunc(work, func(s catalog.SeriesRef) bool { return match.SameSeries(s.Name, main) })
	others := []catalog.SeriesRef{}
	for i, s := range work {
		if !match.SameSeries(s.Name, main) {
			if !(unlisted && i == 0) {
				others = append(others, s)
			}
		} else if !found {
			// A work listed twice in its main series keeps its first position.
			mainPos, found = catalog.FormatSeriesRange(s.Position, s.PositionEnd), true
		}
	}
	plan(scope, catalog.FieldSeriesIndex, mainPos, st.Fields[catalog.FieldSeriesIndex], set)
	// A main series the community doesn't list keeps its position: the book's own
	// spelling of the work's main series, numbered as the community numbers it.
	// The files' position comes back only beside the series it numbers (or when
	// the files have none): never the tags' #3 of one series beside another.
	if files := st.Fields[catalog.FieldSeriesIndex].Scanned; mainPos == "" && !unlisted &&
		(files == "" || match.SameSeries(st.Fields[catalog.FieldSeries].Scanned, main)) {
		stale(catalog.FieldSeriesIndex)
	}
	// In stored form (each name once, within the bounds), as the book's own value
	// is compared and as an apply would store it.
	more := catalog.CleanOverride(catalog.FieldMoreSeries, catalog.EncodeMoreSeries(others))
	plan(scope, catalog.FieldMoreSeries, more, st.Fields[catalog.FieldMoreSeries], set)
	// Only a community that lists no other series takes them away: a list it
	// offers that can't be stored (too many, a name too long) is no answer.
	if len(others) == 0 {
		stale(catalog.FieldMoreSeries)
	}
}

// planFor is what applying item it (its proposal) to a book in state st writes for a
// run in mode: a repick's ASIN (whatever the scope), else Plan's (a refresh's
// proposal carries no identifier, so its Plan writes none: refreshBook), for a
// refresh only while the book still has the identifier it was found by, and for
// a match only while the book is still unmatched (matchedSince).
func planFor(mode, scope string, st *catalog.MatchState, it *catalog.MatchRunItem) Writes {
	p := it.Proposal
	switch mode {
	case catalog.MatchModeRepick:
		return Writes{Set: PlanRepick(st, p)}
	case catalog.MatchModeRefresh:
		if !stillFoundBy(st, p.FoundBy) {
			return Writes{}
		}
	case catalog.MatchModeMatch:
		if matchedSince(st, it) {
			return Writes{}
		}
	}
	return Plan(scope, st, p)
}

// matchedSince reports whether a match run's book has an ASIN or ISBN now. The
// run took only books with neither (UnmatchedBooks), so one has been matched
// since: by hand (from the review or the book page), by another admin or the
// Bridge's Audible backfill, or by a rescan reading one from an Audiobookshelf
// metadata.json. The run's match never writes over it, whatever the scope. An
// item whose own apply a stop cut short after its fields went in
// (DetailCoverPending) holds the run's identifier, not one matched since.
func matchedSince(st *catalog.MatchState, it *catalog.MatchRunItem) bool {
	if it.Detail == DetailCoverPending {
		return false
	}
	return st.Fields[catalog.FieldASIN].Value != "" || st.Fields[catalog.FieldISBN].Value != ""
}

// stillFoundBy reports whether the book still has the identifier a refresh or
// a repick found its record by (Proposal.FoundBy): a book matched again since
// (by hand, to another record) is not overwritten with the old one's details.
func stillFoundBy(st *catalog.MatchState, foundBy string) bool {
	kind, id, ok := strings.Cut(foundBy, ":")
	if !ok {
		return false
	}
	field := map[string]string{"asin": catalog.FieldASIN, "isbn": catalog.FieldISBN}[kind]
	return field != "" && catalog.CleanOverride(field, st.Fields[field].Value) == id
}

// PlanRepick is what applying a repick item writes: its ASIN, while the book's
// ASIN is still the community override the run found (an admin who since edited
// it, or reverted it to the tags, keeps theirs), and still the ASIN the run
// looked up (p.FoundBy): a book matched again since by hand, to another record,
// is a community override too, and keeps that match. An item recorded before
// FoundBy was (none) is judged by the source alone.
func PlanRepick(st *catalog.MatchState, p catalog.MatchProposal) map[string]string {
	cur := st.Fields[catalog.FieldASIN]
	asin := p.Values[catalog.FieldASIN]
	if asin == "" || !cur.Locked || cur.Source != catalog.SourceCommunity || cur.Value == asin {
		return map[string]string{}
	}
	if p.FoundBy != "" && !stillFoundBy(st, p.FoundBy) {
		return map[string]string{}
	}
	return map[string]string{catalog.FieldASIN: asin}
}

// lengthFits reports whether a proposal's recording may be the book's edition by
// length: within meta.PlausibleRuntimeOff of the book's seconds, or either length
// unknown. Nothing is applied unasked otherwise (Classify, a refresh).
func lengthFits(p catalog.MatchProposal, seconds float64) bool {
	off, known := meta.RuntimeOff(p.RuntimeMin, seconds)
	return !known || off <= meta.PlausibleRuntimeOff
}

// Classify sorts a book's candidates (best first) into an outcome: confident
// (OutcomeAuto), worth a look (OutcomeReview) or none (no candidate, or none
// scoring ReviewFloor on metaserve's scale, which only a candidate with Reasons
// is), with the best score and the next one's. p is the
// proposal of the best and seconds the book's length (0 = unknown).
func Classify(cands []meta.MatchCandidate, p catalog.MatchProposal, seconds float64) (outcome string, score, runnerUp int) {
	if len(cands) == 0 {
		return catalog.OutcomeNone, 0, 0
	}
	score = cands[0].Score
	if len(cands) > 1 {
		runnerUp = cands[1].Score
	}
	identified := p.Values[catalog.FieldASIN] != "" || p.Values[catalog.FieldISBN] != ""
	lead := score-runnerUp >= AutoLead
	if cands[0].IdentifierHit() {
		lead = len(cands) == 1 || !cands[1].IdentifierHit()
	}
	switch {
	case score >= AutoScore && lead && identified && lengthFits(p, seconds):
		return catalog.OutcomeAuto, score, runnerUp
	case score < ReviewFloor && cands[0].Reasons != nil:
		return catalog.OutcomeNone, score, runnerUp
	}
	return catalog.OutcomeReview, score, runnerUp
}
