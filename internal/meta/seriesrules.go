package meta

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"
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
// qualifier describes (editionKind.holds) and by no other. A name that only
// extends another ("Discworld: Tiffany Aching") is a series of its own.
//
// judgeSeries applies the rules; workSeries turns its verdicts into the series a
// book of each recording is in (a match candidate's), and railMains orders a
// work's rails by them.

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

// foreignSeries reports whether series id is no series a book of work d is in,
// by the records in facts: its language differs from the work's, or it
// translates another of the work's series that is not itself in another
// language than the work (a German work listed in "Discworld" and in its
// translation "Scheibenwelt" is in Scheibenwelt). Facts not known (no record,
// or no language) never count against a series.
func foreignSeries(facts map[string]*seriesFacts, id string, d *upstreamWorkDetail) bool {
	f := facts[id]
	if f == nil {
		return false
	}
	if otherLanguage(f, d) {
		return true
	}
	for _, t := range f.TranslationOf {
		if t != id && !otherLanguage(facts[t], d) && slices.ContainsFunc(d.Series, func(r upstreamSeriesRef) bool { return r.ID == t }) {
			return true
		}
	}
	return false
}

// otherLanguage reports whether a series' record says it is in another language
// than work d (primary subtags; a language not known agrees).
func otherLanguage(f *seriesFacts, d *upstreamWorkDetail) bool {
	if f == nil {
		return false
	}
	a, b := model.PrimarySubtag(f.Language), model.PrimarySubtag(d.Language)
	return a != "" && b != "" && a != b
}

// editionKind is what an edition's qualifier names. Only the recording kinds
// (abridged and later) say which recordings hold it.
type editionKind int

const (
	notEdition editionKind = iota
	// editionOther: a qualifier this doesn't read as a kind of recording - a
	// language edition, an ordering note ("Chronological"), anything else. It
	// holds no recording.
	editionOther
	// editionUnabridged repeats its base (the unabridged recordings are the base
	// series' own), so it holds no recording either.
	editionUnabridged
	editionAbridged
	editionFullCast
	editionNarrator
)

var (
	// "Discworld (abridged)", "Harry Potter [Full Cast]": a trailing bracketed
	// qualifier.
	editionBracketRE = regexp.MustCompile(`^(.*\S)\s*[(\[]([^()\[\]]+)[)\]]\s*$`)
	// "Harry Potter - Narrated by Stephen Fry", "Harry Potter: Gesprochen von
	// Rufus Beck": a narrator named after a spaced dash or a colon.
	editionNarratorRE = regexp.MustCompile(`(?i)^(.*\S)(?:\s*:|\s+[-\x{2013}\x{2014}])\s+((?:` + narratedBy + `)\s+.+)$`)

	narratedBy = `(?:narrated|read|performed)\s+by|gesprochen\s+von|lu\s+par|letto\s+da|le[ií]do\s+por|narrado\s+por`
	narratorRE = regexp.MustCompile(`(?i)(?:` + narratedBy + `)\s+(.+)`)
	// A word that starts the qualifier or follows a non-letter (\b is ASCII-only,
	// so it would miss "íntegro").
	unabridgedRE = regexp.MustCompile(`(?i)(?:^|\PL)(?:unabridged|ungek[uü]rzt|int[eé]gral|[ií]ntegr[oa])`)
	abridgedRE   = regexp.MustCompile(`(?i)(?:^|\PL)(?:abridged|gek[uü]rzt|abr[eé]g[eé]|abreviad[oa]|ridott[oa]|condensed)`)
	fullCastRE   = regexp.MustCompile(`(?i)\b(?:full[- ]?cast|dramati[sz]|radio|audio\s*drama|h[oö]rspiel)`)
	castMemberRE = regexp.MustCompile(`(?i)\bfull[- ]?cast\b`)
)

// fullCastVoices is the narrators at which a recording is a full-cast production.
const fullCastVoices = 4

// kindOf reads an edition's qualifier: a narrator first (it says the most:
// "Unabridged, Narrated by Stephen Fry"), then unabridged before abridged (the
// one word holds the other).
func kindOf(qualifier string) editionKind {
	switch {
	case narratorRE.MatchString(qualifier):
		return editionNarrator
	case unabridgedRE.MatchString(qualifier):
		return editionUnabridged
	case abridgedRE.MatchString(qualifier):
		return editionAbridged
	case fullCastRE.MatchString(qualifier):
		return editionFullCast
	}
	return editionOther
}

// holds reports whether recording rec is the kind of recording an edition of
// kind k with qualifier names: abridged, a full cast, the named narrator.
func (k editionKind) holds(qualifier string, rec *upstreamRecording) bool {
	switch k {
	case editionAbridged:
		return rec.Abridged
	case editionFullCast:
		return len(rec.Narrators) >= fullCastVoices ||
			slices.ContainsFunc(rec.Narrators, func(n upstreamPersonRef) bool { return castMemberRE.MatchString(n.Name) })
	case editionNarrator:
		want := match.Fold(narratorRE.FindStringSubmatch(qualifier)[1])
		return slices.ContainsFunc(rec.Narrators, func(n upstreamPersonRef) bool { return match.NamesMatch(want, match.Fold(n.Name)) })
	}
	return false
}

// editionQualifier returns what makes name an edition of another of the work's
// series (bases): its "narrated by" tail, or its bracketed qualifier, when the
// rest of the name is a base's. ok is false for a series of its own.
func editionQualifier(name string, bases []string) (qualifier string, ok bool) {
	m := editionNarratorRE.FindStringSubmatch(name)
	if m == nil {
		m = editionBracketRE.FindStringSubmatch(name)
	}
	if m == nil {
		return "", false
	}
	key := match.SeriesKey(m[1])
	if key == "" || !slices.ContainsFunc(bases, func(b string) bool { return match.SeriesKey(b) == key }) {
		return "", false
	}
	return m[2], true
}

// judgedSeries is one of a work's series with what the rules make of it for a
// book: Dropped (no series a book of the work is in), else an edition (Kind) held
// only by Recordings, or a series every recording is in (Kind notEdition).
// Foreign says its record puts it in another language, or makes it a
// translation (foreignSeries); a foreign series is Dropped and never an edition.
type judgedSeries struct {
	Ref        upstreamSeriesRef
	Kind       editionKind
	Recordings []string
	Dropped    bool
	Foreign    bool
}

// judgeSeries applies the rules to a work's memberships, in order, leaving out
// the variant reading orders (they are alternate views of a rail, never a
// series of their own) and a series listed twice (its first position stays).
// facts may lack a series: unknown facts drop nothing.
func judgeSeries(d *upstreamWorkDetail, facts map[string]*seriesFacts) []judgedSeries {
	var out []judgedSeries
	var bases []string
	seen := map[string]bool{}
	for _, ref := range d.Series {
		if ref.OrderingOf != "" || seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		foreign := foreignSeries(facts, ref.ID, d)
		j := judgedSeries{Ref: ref, Dropped: foreign, Foreign: foreign}
		if !j.Dropped {
			bases = append(bases, ref.Name)
		}
		out = append(out, j)
	}
	// An edition's name is its base's plus a qualifier, so it never reads as an
	// edition of itself.
	for i := range out {
		j := &out[i]
		q, ok := editionQualifier(j.Ref.Name, bases)
		if j.Dropped || !ok {
			continue
		}
		j.Kind = kindOf(q)
		for k := range d.Recordings {
			if j.Kind.holds(q, &d.Recordings[k]) {
				j.Recordings = append(j.Recordings, d.Recordings[k].ID)
			}
		}
		j.Dropped = len(j.Recordings) == 0
	}
	return out
}

// workSeries is the series a book of a work is in: those of every recording
// (the work's own, in its order) and the editions, each held by some recordings.
type workSeries struct {
	all      []MatchSeries
	editions []judgedSeries
}

func newWorkSeries(d *upstreamWorkDetail, facts map[string]*seriesFacts) workSeries {
	ws := workSeries{all: []MatchSeries{}}
	for _, j := range judgeSeries(d, facts) {
		switch {
		case j.Dropped:
		case j.Kind != notEdition:
			ws.editions = append(ws.editions, j)
		default:
			ws.all = append(ws.all, MatchSeries{ID: j.Ref.ID, Name: j.Ref.Name, Position: j.Ref.Position})
		}
	}
	return ws
}

// of is the series a book that is recording recordingID is in, its main one
// first: the first edition named for that recording (the series named for
// exactly this recording), else the work's first series, then the rest.
func (ws workSeries) of(recordingID string) []MatchSeries {
	var own []MatchSeries
	for _, e := range ws.editions {
		if slices.Contains(e.Recordings, recordingID) {
			own = append(own, MatchSeries{ID: e.Ref.ID, Name: e.Ref.Name, Position: e.Ref.Position})
		}
	}
	if len(own) == 0 {
		return ws.all
	}
	return slices.Concat(own[:1], ws.all, own[1:])
}

// railMains orders and filters a work's rail families (familyMains) for the
// recording a book is (recordingID, "" = not known), before the rail cap
// (maxSeriesRails) is spent: an edition named for a kind of recording only when
// the book is that recording, and then first (it is the book's main series, as
// workSeries.of makes it), and no series already known to be
// foreign (facts: what the cache holds; the records aren't read here). Every
// other family keeps its place: a variant-only work's variant, and an edition
// named for anything else ("(Chronological)" from a metaserve that predates
// ordering_of), as rails always showed them.
func railMains(mains []upstreamSeriesRef, d *upstreamWorkDetail, recordingID string, facts map[string]*seriesFacts) []upstreamSeriesRef {
	verdicts := map[string]judgedSeries{}
	for _, j := range judgeSeries(d, facts) {
		verdicts[j.Ref.ID] = j
	}
	var out, editions []upstreamSeriesRef
	for _, ref := range mains {
		j, ok := verdicts[ref.ID]
		switch {
		case !ok:
			out = append(out, ref)
		case j.Kind == notEdition || j.Kind == editionOther:
			if !j.Foreign {
				out = append(out, ref)
			}
		case slices.Contains(j.Recordings, recordingID):
			editions = append(editions, ref)
		}
	}
	return append(editions, out...)
}

// cachedFacts is the facts the cache holds for d's series, none fetched.
func (s *Service) cachedFacts(d *upstreamWorkDetail) map[string]*seriesFacts {
	out := map[string]*seriesFacts{}
	for _, ref := range d.Series {
		if f, hit, err := cacheGet[seriesFacts](s.cache, nsSeries.key(ref.ID)); hit && err == nil && f != nil {
			out[ref.ID] = f
		}
	}
	return out
}

// maxSeriesFacts bounds the series records one match reads for their facts,
// and seriesFactsTimeout how long it waits for them. The facts only ever drop a
// series, and one not read drops nothing, so a work listed in dozens of series
// (odd or hostile data) or a slow series endpoint costs some facts, never a
// burst of GETs on the shared community service or a dialog held for the whole
// compose budget.
const (
	maxSeriesFacts     = 16
	seriesFactsTimeout = 5 * time.Second
)

// seriesFactsFor reads the facts of the series the works name that a book can
// be in (the variant orders aside) and the facts could rule on - a work with
// no language and a single series has nothing to rule on -, each once, at most
// maxSeriesFacts, concurrently (bounded by workSem), from the cache when they
// are there. A series whose record can't be read is left out, which drops
// nothing.
func (s *Service) seriesFactsFor(ctx context.Context, works []*upstreamWorkDetail) map[string]*seriesFacts {
	var ids []string
	for _, d := range works {
		var own []string
		for _, ref := range d.Series {
			if ref.OrderingOf == "" && ref.ID != "" && !slices.Contains(own, ref.ID) {
				own = append(own, ref.ID)
			}
		}
		if d.Language == "" && len(own) < 2 {
			continue
		}
		for _, id := range own {
			if len(ids) < maxSeriesFacts && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, seriesFactsTimeout)
	defer cancel()
	facts := make([]*seriesFacts, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { facts[i] = s.seriesFacts(ctx, id) })
	}
	wg.Wait()
	out := make(map[string]*seriesFacts, len(ids))
	for i, f := range facts {
		if f != nil {
			out[ids[i]] = f
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
