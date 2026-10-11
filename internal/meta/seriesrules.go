package meta

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Which of a work's series a book of it is in. The community lists every series
// a work belongs to, and some of those are not series the book is in:
//
//   - a variant READING ORDER (ordering_of) of another series - the same books
//     in another order, offered as an alternate view of its rail;
//   - a TRANSLATION (translation_of) of a series the work is also in, or a series
//     in another language than the work;
//   - an EDITION of another of the work's series: the same series named again for
//     one kind of recording ("Discworld (abridged)", "Harry Potter (Full-Cast
//     Editions)", "Harry Potter - Narrated by Stephen Fry"), which a book is in
//     only as that recording.
//
// The data states the first two (series records carry ordering_of,
// translation_of and a derived language). Editions are not linked to their
// recordings anywhere the community API serves, so they are read from the name:
// a series named exactly like another of the work's series plus a bracketed
// qualifier (or a "narrated by" tail) is an edition, held by the recordings the
// qualifier describes (editionHolds) and by no other. A name that only extends
// another ("Discworld: Tiffany Aching") is a series of its own.
//
// workSeries applies the rules to a match candidate (MatchCandidate.Series),
// SeriesFor picks the book's main series for the recording it is, and
// railMains orders a work's rails by the same rules.

// seriesFacts is what a series' own record says about it that a work's
// membership doesn't: its derived language ("" where its members tie) and the
// series it translates.
type seriesFacts struct {
	Language      string
	TranslationOf []string
}

// factsOf reads a series detail's facts.
func factsOf(sd *upstreamSeriesDetail) *seriesFacts {
	f := &seriesFacts{Language: sd.Language}
	for _, t := range sd.TranslationOf {
		if t.ID != "" {
			f.TranslationOf = append(f.TranslationOf, t.ID)
		}
	}
	return f
}

// foreignSeries reports whether a series with facts f is no series a work in
// workLang is in: it translates one of the work's other series (onWork lists
// every series id the work names), or its language differs from the work's.
// Facts not known (nil, or no language) never count against a series.
func foreignSeries(f *seriesFacts, id, workLang string, onWork []string) bool {
	if f == nil {
		return false
	}
	for _, t := range f.TranslationOf {
		if t != id && slices.Contains(onWork, t) {
			return true
		}
	}
	return !sameLanguage(f.Language, workLang)
}

// sameLanguage compares two BCP 47 tags by their primary subtag; an unknown one
// (empty) agrees with anything.
func sameLanguage(a, b string) bool {
	primary := func(tag string) string {
		tag, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		return tag
	}
	a, b = primary(a), primary(b)
	return a == "" || b == "" || a == b
}

var (
	// "Discworld (abridged)", "Harry Potter [Full Cast]": a trailing bracketed
	// qualifier.
	editionBracketRE = regexp.MustCompile(`^(.*\S)\s*[(\[]([^()\[\]]+)[)\]]\s*$`)
	// "Harry Potter - Narrated by Stephen Fry", "Harry Potter - Gesprochen von
	// Rufus Beck": a narrator named after a dash or a colon.
	editionNarratorRE = regexp.MustCompile(`(?i)^(.*\S)\s+[-:\x{2013}\x{2014}]\s+((?:` + narratedBy + `)\s+.+)$`)

	narratedBy = `(?:narrated|read|performed)\s+by|gesprochen\s+von|lu\s+par|letto\s+da|le[ií]do\s+por|narrado\s+por`
	narratorRE = regexp.MustCompile(`(?i)(?:` + narratedBy + `)\s+(.+)`)
	// A word that starts the qualifier or follows a non-letter (\b is ASCII-only,
	// so it would miss "íntegro").
	unabridgedRE  = regexp.MustCompile(`(?i)(?:^|\PL)(?:unabridged|ungek[uü]rzt|int[eé]gral|[ií]ntegr[oa])`)
	abridgedRE    = regexp.MustCompile(`(?i)(?:^|\PL)(?:abridged|gek[uü]rzt|abr[eé]g[eé]|abreviad[oa]|ridott[oa]|condensed)`)
	fullCastRE    = regexp.MustCompile(`(?i)\b(?:full[- ]?cast|dramati[sz]|radio|audio\s*drama|h[oö]rspiel)`)
	castMemberRE  = regexp.MustCompile(`(?i)\bfull[- ]?cast\b`)
	fullCastVoice = 4 // narrators at which a recording is a full-cast production
)

// editionQualifier returns what makes name an edition of another of the work's
// series (bases): its bracketed qualifier, or its "narrated by" tail, when the
// rest of the name is a base's. ok is false for a series of its own.
func editionQualifier(name string, bases []string) (qualifier string, ok bool) {
	stem := ""
	if m := editionNarratorRE.FindStringSubmatch(name); m != nil {
		stem, qualifier = m[1], m[2]
	} else if m := editionBracketRE.FindStringSubmatch(name); m != nil {
		stem, qualifier = m[1], m[2]
	} else {
		return "", false
	}
	key := match.SeriesKey(stem)
	if key == "" || !slices.ContainsFunc(bases, func(b string) bool { return match.SeriesKey(b) == key }) {
		return "", false
	}
	return qualifier, true
}

// recordingKind reports whether an edition's qualifier names a kind of
// recording (editionHolds reads it), rather than anything else: a language
// edition, an ordering note ("Chronological") or a word this doesn't read.
func recordingKind(qualifier string) bool {
	for _, re := range []*regexp.Regexp{unabridgedRE, abridgedRE, fullCastRE, narratorRE} {
		if re.MatchString(qualifier) {
			return true
		}
	}
	return false
}

// editionHolds reports whether recording rec is the kind of recording an
// edition's qualifier names: abridged, a full cast, a narrator (an unabridged
// edition repeats its base, so it holds none).
// Anything else (!recordingKind) holds no recording, so the edition is no
// series of the book's.
func editionHolds(qualifier string, rec *upstreamRecording) bool {
	switch {
	case unabridgedRE.MatchString(qualifier):
		// The base series is the unabridged recordings' already: an
		// "(Unabridged)" edition names nothing a book isn't in by its base.
		return false
	case abridgedRE.MatchString(qualifier):
		return rec.Abridged
	case fullCastRE.MatchString(qualifier):
		if len(rec.Narrators) >= fullCastVoice {
			return true
		}
		return slices.ContainsFunc(rec.Narrators, func(n upstreamPersonRef) bool { return castMemberRE.MatchString(n.Name) })
	}
	if m := narratorRE.FindStringSubmatch(qualifier); m != nil {
		want := match.Fold(m[1])
		return want != "" && slices.ContainsFunc(rec.Narrators, func(n upstreamPersonRef) bool {
			got := match.Fold(n.Name)
			return got != "" && (got == want || strings.Contains(want, got) || strings.Contains(got, want))
		})
	}
	return false
}

// judgedSeries is one of a work's series with what the rules make of it for a
// book: dropped (no series a book of the work is in), or held only by some
// recordings (an edition), or by every one (Recordings nil, Edition false).
// RecordingKind says the edition is named for a kind of recording, rather than
// for something this can't place (only those reorder rails: railMains).
type judgedSeries struct {
	Ref           upstreamSeriesRef
	Edition       bool
	RecordingKind bool
	Recordings    []string
	Dropped       bool
}

// judgeSeries applies the rules to a work's memberships, in order, leaving out
// the variant reading orders (they are alternate views of a rail, never a
// series of their own) and a series listed twice (its first position stays).
// facts may be nil, or lack a series: unknown facts drop nothing.
func judgeSeries(d *upstreamWorkDetail, facts map[string]*seriesFacts) []judgedSeries {
	var onWork []string
	for _, ref := range d.Series {
		onWork = append(onWork, ref.ID)
	}
	var out []judgedSeries
	for _, ref := range d.Series {
		if ref.OrderingOf != "" || slices.ContainsFunc(out, func(j judgedSeries) bool { return j.Ref.ID == ref.ID }) {
			continue
		}
		out = append(out, judgedSeries{Ref: ref, Dropped: foreignSeries(facts[ref.ID], ref.ID, d.Language, onWork)})
	}
	var bases []string
	for _, j := range out {
		if !j.Dropped {
			bases = append(bases, j.Ref.Name)
		}
	}
	for i := range out {
		j := &out[i]
		if j.Dropped {
			continue
		}
		others := slices.DeleteFunc(slices.Clone(bases), func(b string) bool { return b == j.Ref.Name })
		q, ok := editionQualifier(j.Ref.Name, others)
		if !ok {
			continue
		}
		j.Edition, j.RecordingKind = true, recordingKind(q)
		for k := range d.Recordings {
			if editionHolds(q, &d.Recordings[k]) {
				j.Recordings = append(j.Recordings, d.Recordings[k].ID)
			}
		}
		j.Dropped = len(j.Recordings) == 0
	}
	return out
}

// workSeries is a candidate's series: the work's series a book of it can be in
// (judgeSeries), the ones every recording is in first, then the editions, each
// naming its recordings.
func workSeries(d *upstreamWorkDetail, facts map[string]*seriesFacts) []MatchSeries {
	out := []MatchSeries{}
	var editions []MatchSeries
	for _, j := range judgeSeries(d, facts) {
		switch {
		case j.Dropped:
		case j.Edition:
			editions = append(editions, MatchSeries{ID: j.Ref.ID, Name: j.Ref.Name, Position: j.Ref.Position, RecordingIDs: j.Recordings})
		default:
			out = append(out, MatchSeries{ID: j.Ref.ID, Name: j.Ref.Name, Position: j.Ref.Position})
		}
	}
	return append(out, editions...)
}

// SeriesFor is the series a book that is recording recordingID ("" = not
// known) of a candidate is in, its main one first: the first edition that
// recording is (the series named for exactly this recording), else the work's
// first series. Editions of other recordings are left out.
// (admin-ui match-model.ts seriesFor is the console's copy.)
func SeriesFor(series []MatchSeries, recordingID string) []MatchSeries {
	out := make([]MatchSeries, 0, len(series))
	main := -1
	for _, s := range series {
		if len(s.RecordingIDs) == 0 {
			out = append(out, s)
			continue
		}
		if recordingID == "" || !slices.Contains(s.RecordingIDs, recordingID) {
			continue
		}
		if main < 0 {
			main = len(out)
		}
		out = append(out, s)
	}
	if main > 0 {
		m := out[main]
		copy(out[1:main+1], out[:main])
		out[0] = m
	}
	return out
}

// railMains orders and filters a work's rail families (familyMains) for the
// recording a book is (recordingID, "" = not known): an edition named for a kind
// of recording (judgeSeries without facts: the series records aren't read yet)
// only when the book is that recording, after the rest, so the cap on rails
// (maxSeriesRails) spends itself on the series the book is in. Every other
// family keeps its place: one whose main view is a variant order (a variant-only
// work), and an edition named for anything else ("(Chronological)" from a
// metaserve that predates ordering_of), as rails always showed them.
func railMains(mains []upstreamSeriesRef, d *upstreamWorkDetail, recordingID string) []upstreamSeriesRef {
	judged := judgeSeries(d, nil)
	verdict := func(id string) (judgedSeries, bool) {
		i := slices.IndexFunc(judged, func(j judgedSeries) bool { return j.Ref.ID == id })
		if i < 0 {
			return judgedSeries{}, false
		}
		return judged[i], true
	}
	var out, editions []upstreamSeriesRef
	for _, ref := range mains {
		j, ok := verdict(ref.ID)
		switch {
		case !ok || !j.RecordingKind:
			out = append(out, ref)
		case recordingID != "" && slices.Contains(j.Recordings, recordingID):
			editions = append(editions, ref)
		}
	}
	return append(out, editions...)
}

// seriesFactsFor reads the facts of every series d names that a book can be in
// (the variant orders aside), from the cache when they are there. A series whose
// record can't be read is left out of the map, which drops nothing.
func (s *Service) seriesFactsFor(ctx context.Context, d *upstreamWorkDetail) map[string]*seriesFacts {
	out := map[string]*seriesFacts{}
	for _, ref := range d.Series {
		if ref.OrderingOf != "" || ref.ID == "" {
			continue
		}
		if _, seen := out[ref.ID]; seen {
			continue
		}
		if f := s.seriesFacts(ctx, ref.ID); f != nil {
			out[ref.ID] = f
		}
	}
	return out
}

// seriesFacts is one series' facts: from the cache, else its series record
// (bounded by workSem, like a work's), cached as long as an enrichment; nil
// when the record can't be read (a failure is not cached).
func (s *Service) seriesFacts(ctx context.Context, id string) *seriesFacts {
	key := nsSeries.key(id)
	if f, hit, err := cacheGet[seriesFacts](s.cache, key); hit {
		if err != nil {
			return nil
		}
		return f
	}
	select {
	case s.workSem <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-s.workSem }()
	sd, err := s.client.series(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && (sd == nil || sd.ID == "")):
		s.cache.putMiss(key, notFoundTTL)
		return nil
	case err != nil:
		return nil
	}
	f := factsOf(sd)
	cachePut(s.cache, key, f, positiveTTL)
	return f
}
