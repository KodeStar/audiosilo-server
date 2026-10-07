package meta

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxSeriesRails caps how many series a work's enrichment fetches. A work is
// rarely in more than one or two series; the cap bounds the fan-out to metaserve
// per lookup regardless of odd data. It bounds ATTEMPTS, not successes, so
// hostile upstream data (a work carrying dozens of series refs) with a failing
// series endpoint can never issue more than this many series requests.
//
// It counts ordering FAMILIES, not series: a primary series and its variant
// reading orders collapse into one rail (see seriesRails), so a work in
// "Narnia" and "Narnia (Chronological)" spends one slot, not two.
const maxSeriesRails = 3

// maxOrderingAlternates caps how many OTHER reading orders of one family are
// fetched as alternates of its rail. Like maxSeriesRails it bounds attempts, so
// one enrichment issues at most maxSeriesRails * (1 + maxOrderingAlternates)
// series GETs however large a family upstream claims to be. The measured
// families hold two or three orderings, so two alternates cover every one.
const maxOrderingAlternates = 2

// composeTimeout bounds one full compose fan-out (lookup + work + up to
// maxSeriesRails rails, each one series call plus up to maxOrderingAlternates;
// every main view is fetched before any alternate, so the optional views never
// starve a rail of the budget).
// Each call already has the client's 5s timeout, but sequentially those could
// sum past the API's 30s request budget; this keeps the whole composition
// comfortably under it. When THIS deadline
// fires while the caller is still live, the parent ctx.Err() stays nil, so the
// failure IS cached as a transport error for errorTTL - exactly the protective
// behavior we want against a degraded-but-alive upstream (it is not re-hammered
// on every cold book).
const composeTimeout = 15 * time.Second

// maxConcurrentWorkFetches bounds how many uncached work-id lookups may be in
// flight upstream at once (cache hits never touch it). Unlike an enrichment -
// keyed by a book this server actually holds - a work id is picked freely by any
// signed-in caller, so each distinct id is one outbound GET to the SHARED
// community metadata service. This is the amplification bound: a burst of
// distinct ids queues here rather than fanning straight out to metaserve. Not
// configurable; it is a courtesy limit on a shared third party, matching the
// api package's transcodeSem precedent.
const maxConcurrentWorkFetches = 4

// maxConcurrentLookups bounds how many uncached identifier lookups the admin
// console's Series cards (WorkIDs) may have in flight upstream at once, across
// every console request (cache hits never touch it, and a book a player has
// enriched is answered from its enrichment). A Series page asks about every
// matched book of a few dozen cards at once, so without a shared bound one
// screen of cards would fan out to the community service in parallel. Like
// maxConcurrentWorkFetches, a courtesy limit on a shared third party; a lookup is
// one small GET, so a queue here drains quickly. A player's Enrich never waits
// here: its lookup is one per opened book (as before WorkIDs existed), and a
// console batch must not be able to time a player's /meta out.
const maxConcurrentLookups = 4

// MetaPersonRef is the {id,name} shape for an author or narrator.
type MetaPersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MetaPosition is a spoiler position on a work's own (edition-independent)
// timeline. Chapter is the logical work chapter; 0 = front matter / prior-book.
type MetaPosition struct {
	Chapter int `json:"chapter"`
}

// MetaCharacter is one community-authored, spoiler-tagged character entry
// (the CC BY-SA layer). Reveal is where it is first disclosed in the work.
type MetaCharacter struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Aliases     []string     `json:"aliases,omitempty"`
	Role        string       `json:"role,omitempty"`
	Reveal      MetaPosition `json:"reveal"`
	Description string       `json:"description,omitempty"`
}

// MetaRecap is one position-keyed "story so far" recap. Through is the position
// it is safe to show at (the listener has finished that chapter).
type MetaRecap struct {
	Through MetaPosition `json:"through"`
	Scope   string       `json:"scope,omitempty"`
	Text    string       `json:"text"`
}

// MetaRecapSummary is the per-work whole-book refresher: a one-paragraph "in
// short" catch-up and a plain statement of how the book ends. It is the
// catch-up payload for a PREVIOUS book in a series - Ending is a full spoiler
// for that work by construction, so a client must only reveal it deliberately.
// Omitted entirely when the work has no summary sidecar.
type MetaRecapSummary struct {
	InShort string `json:"in_short,omitempty"`
	Ending  string `json:"ending,omitempty"`
}

// MetaCommunityDescription is the community-written, spoiler-free description of
// a work (the CC BY-SA layer). It is kept apart from MetaWork.Description - the
// CC0 core's publisher-style blurb - because the two carry different licenses
// and a client must be able to credit this one. License is upstream's license
// code (e.g. "CC-BY-SA-4.0"); Text is never empty.
type MetaCommunityDescription struct {
	Text    string `json:"text"`
	License string `json:"license,omitempty"`
}

// The credit line every CC BY-SA layer carries (see MetaAttribution). Declared
// once, here, so the legal text a client renders is the server's, never
// reassembled per client; a license bump edits these lines and nothing else.
// The URL matches metaserve's own ccBySAURL.
const (
	attributionContributors = "AudioSilo Meta community contributors"
	attributionLicense      = "CC BY-SA 4.0"
	attributionLicenseURL   = "https://creativecommons.org/licenses/by-sa/4.0/"
)

// MetaAttribution is the credit the CC BY-SA layer (characters, recaps,
// recap_summary, community_description) requires wherever it is shown. The
// server writes it so every client renders the same legal text; SourceURL is
// the work's page on the metadata site (the envelope's web_url).
type MetaAttribution struct {
	Credit     string `json:"credit"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url"`
	SourceURL  string `json:"source_url"`
}

// MetaWork is the abstract book in an enrichment envelope. It is also the
// standalone payload of Service.Work (a work-id-addressed lookup for a sibling
// book in a series).
type MetaWork struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Subtitle       string            `json:"subtitle,omitempty"`
	Authors        []MetaPersonRef   `json:"authors"`
	Language       string            `json:"language"`
	FirstPublished string            `json:"first_published,omitempty"`
	Description    string            `json:"description,omitempty"`
	Characters     []MetaCharacter   `json:"characters,omitempty"`
	Recaps         []MetaRecap       `json:"recaps,omitempty"`
	RecapSummary   *MetaRecapSummary `json:"recap_summary,omitempty"`
	// CommunityDescription is the CC BY-SA description, separate from
	// Description (see MetaCommunityDescription).
	CommunityDescription *MetaCommunityDescription `json:"community_description,omitempty"`
	// Attribution is present iff the work carries any CC BY-SA content (see
	// carriesCommunityContent), so a client shows the credit exactly when it
	// shows content that needs it.
	Attribution *MetaAttribution `json:"attribution,omitempty"`
}

// MetaRecording is the specific narration/production matched by the lookup.
type MetaRecording struct {
	ID          string          `json:"id"`
	Narrators   []MetaPersonRef `json:"narrators"`
	Abridged    bool            `json:"abridged,omitempty"`
	RuntimeMin  int             `json:"runtime_min,omitempty"`
	ReleaseDate string          `json:"release_date,omitempty"`
	Publisher   string          `json:"publisher,omitempty"`
	CoverURL    string          `json:"cover_url,omitempty"`
	// ChapterCount is the recording's chapter count; omitted when 0/unknown.
	ChapterCount int `json:"chapter_count,omitempty"`
}

// MetaSeriesWork is one entry of a series rail. It carries its own web_url so the
// client never constructs metadata site URLs itself.
type MetaSeriesWork struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Position string          `json:"position"`
	Authors  []MetaPersonRef `json:"authors"`
	CoverURL string          `json:"cover_url,omitempty"`
	WebURL   string          `json:"web_url"`
	// Local is the book the CALLER can open that is this work (PlaceLocal). It
	// is set per request on a copy of the rails, never on an envelope the cache
	// holds.
	Local *MetaLocal `json:"local,omitempty"`
}

// MetaSeries is a full ordered series rail, including the current work.
//
// One rail is one ordering FAMILY: a primary series plus the variant reading
// orders (chronological, recommended) that name it. The top-level
// ID/Name/Position/Works are always the family's MAIN view - the primary when
// the work sits in it, else the variant that holds it - so a shipped player that
// predates orderings reads exactly one rail per family in the primary order and
// cannot be spoiled by a second rail listing the same books in another order.
// Ordering, OrderingOf and Orderings are additive: a client keys the family as
// `ordering_of || id` and offers Orderings as alternate views of the same rail.
type MetaSeries struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Position string           `json:"position"` // the current work's position in this series
	Works    []MetaSeriesWork `json:"works"`
	// Ordering is the reading order the main view states
	// (publication/chronological/recommended), omitted when unstated.
	Ordering string `json:"ordering,omitempty"`
	// OrderingOf is set only when the main view is itself a VARIANT (a work
	// placed by no other order of its family): the primary series' id.
	OrderingOf string `json:"ordering_of,omitempty"`
	// Orderings are the family's OTHER reading orders, in metaserve's family
	// order (primary first, then variants by id), the main view excluded.
	Orderings []MetaSeriesOrdering `json:"orderings,omitempty"`
}

// MetaSeriesOrdering is one alternate reading order of a rail's family. Position
// is the current work's position in THIS order, and is empty when the order does
// not place the work at all (a chronological list can omit a book the
// publication order holds, and vice versa). Works are built exactly as a rail's.
type MetaSeriesOrdering struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Ordering   string           `json:"ordering,omitempty"`
	OrderingOf string           `json:"ordering_of,omitempty"`
	Position   string           `json:"position,omitempty"`
	Works      []MetaSeriesWork `json:"works"`
}

// Enrichment is the composed envelope returned on a match. Matched is always true
// here; the api handler emits {"matched": false} for the no-match / no-ids cases.
type Enrichment struct {
	Matched   bool           `json:"matched"`
	Work      *MetaWork      `json:"work,omitempty"`
	Recording *MetaRecording `json:"recording,omitempty"`
	Series    []MetaSeries   `json:"series,omitempty"`
	WebURL    string         `json:"web_url"`
	// Previous is the works before this one in its series, nearest first
	// (GET /libraries/{id}/meta?include=previous; see Service.Previous). It is
	// set per request on a copy, never on an envelope the cache holds.
	Previous []*MetaWork `json:"previous,omitempty"`
}

// Service composes book enrichments from the metadata API, with a bounded TTL
// cache in front so a repeated lookup (and a down upstream) is cheap. A nil
// *Service means the feature is off; callers must guard on that.
type Service struct {
	client  *client
	baseURL string // metaserve base URL (no trailing slash) for building web_url
	cache   *cache
	// store is the persistent second level behind cache (see Store); nil keeps
	// the cache in memory only.
	store Store
	// composeBudget bounds one full compose fan-out. Defaults to composeTimeout;
	// a field only so tests can shrink it.
	composeBudget time.Duration
	// workSem bounds concurrent uncached work-id fetches upstream (see
	// maxConcurrentWorkFetches).
	workSem chan struct{}
	// lookupSem bounds WorkIDs' concurrent uncached identifier lookups (see
	// maxConcurrentLookups).
	lookupSem chan struct{}
	// flights are WorkIDs' lookups in flight, by "l:" key, so concurrent misses
	// of one identifier share one upstream GET (see sharedLookup).
	flightMu sync.Mutex
	flights  map[string]*lookupFlight
	// health caches Ping's answer.
	health healthCache
	// covers fetches community cover images (FetchCover), at most
	// maxConcurrentCoverFetches at once (coverSem).
	covers   *http.Client
	coverSem chan struct{}
	now      func() time.Time
	// matchUnsupportedUntil (unix nanoseconds) is when works/match is next
	// tried after metaserve answered it as an unknown route (Candidates).
	matchUnsupportedUntil atomic.Int64
}

// NewService builds a Service for the given metaserve base URL. now may be nil
// (defaults to time.Now); it is injectable so tests can drive cache TTLs.
func NewService(baseURL string, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	base := strings.TrimRight(baseURL, "/")
	return &Service{
		client:        newClient(base),
		baseURL:       base,
		cache:         newCache(now),
		composeBudget: composeTimeout,
		workSem:       make(chan struct{}, maxConcurrentWorkFetches),
		lookupSem:     make(chan struct{}, maxConcurrentLookups),
		flights:       map[string]*lookupFlight{},
		covers:        newCoverClient(publicAddr),
		coverSem:      make(chan struct{}, maxConcurrentCoverFetches),
		now:           now,
	}
}

// Enrich resolves a book's asin (preferred) or isbn to a composed enrichment.
// It returns ErrNotFound when there is no match, and a non-nil, non-ErrNotFound
// error when the upstream is unreachable. Results (including "not found" and
// transport errors) are cached so a hot path or a down upstream is not re-hit.
// The identifiers are normalized (normalizeASIN, normalizeISBN) first, so the
// spellings of one identifier share one cache entry and one upstream lookup.
//
// With a Store (SetStore) the cache has a persistent second level: answers
// survive a restart, and a positive one is served past its TTL when the
// upstream is down (see Store for exactly what is kept).
//
// The returned *Enrichment is shared with the cache and other callers - treat
// it as immutable; never modify it (or anything it points to) after the call.
func (s *Service) Enrich(ctx context.Context, asin, isbn string) (*Enrichment, error) {
	asin, isbn = normalizeASIN(asin), normalizeISBN(isbn)
	key := cacheKey(asin, isbn)
	if key == "" {
		return nil, ErrNotFound
	}
	// A hit resolves to the memoised outcome directly: err is nil for a positive
	// result, ErrNotFound for a cached "no match", or the cached transport error.
	if result, hit, err := cacheGet[Enrichment](s.cache, key); hit {
		return result, err
	}
	// Then the persistent store: a fresh row answers as memory would have (and
	// warms it for the rest of its TTL); a stale positive row is held back as the
	// fallback should the upstream fail below.
	stored, expires, fresh := readStored[Enrichment](ctx, s, key)
	if fresh {
		cachePutUntil(s.cache, key, stored, expires)
		if stored == nil {
			return nil, ErrNotFound
		}
		return stored, nil
	}

	// Run the whole fan-out under its own deadline (see composeTimeout): a
	// slow-but-alive upstream must not eat the API's whole request budget, and
	// hitting THIS deadline (parent still live) is cached like any upstream
	// failure below, so a degraded upstream is not re-hammered per cold book.
	cctx, cancel := context.WithTimeout(ctx, s.composeBudget)
	defer cancel()
	result, complete, err := s.compose(cctx, asin, isbn)
	switch {
	case errors.Is(err, ErrNotFound):
		s.cache.putMiss(key, notFoundTTL)
		saveStored[Enrichment](ctx, s, key, nil, notFoundTTL)
		return nil, ErrNotFound
	case err != nil:
		// Only cache failures the UPSTREAM caused. When the caller's own context
		// is done (the client aborted the request or its deadline passed -
		// routine: the player cancels in-flight fetches on navigation), caching
		// the resulting error would poison this book's enrichment with 502s for
		// the whole error TTL while upstream is perfectly healthy. The PARENT
		// ctx.Err() cleanly discriminates the two: it is nil for a genuine
		// upstream failure, including the client's per-call 5s timeout or the
		// compose deadline firing under a live caller.
		if ctx.Err() == nil {
			// An upstream failure with a persisted positive answer, however
			// stale, serves that answer: it is held in memory for errorTTL
			// (exactly as the error would have been), so the upstream is asked
			// again soon, and the row itself is left as it was. An error is
			// never persisted.
			if stored != nil {
				cachePut(s.cache, key, stored, errorTTL)
				return stored, nil
			}
			s.cache.putError(key, err)
		}
		return nil, err
	case !complete:
		// A usable envelope, but at least one series rail failed transiently.
		// Caching it for the full positive TTL would hide "more in this series"
		// for a day on a blip, so hold it only briefly (errorTTL) and retry
		// soon - in the store too, so a restart retries it as soon. And if the
		// caller's context is done, the missing rails were caused by the
		// CALLER's cancellation mid-fan-out - cache nothing at all, same
		// reasoning as the error branch above.
		if ctx.Err() == nil {
			cachePut(s.cache, key, result, errorTTL)
			// Not over a stored positive answer, though: that one, stale but
			// with every rail it had, stays the outage fallback (a row that
			// lives two minutes adds nothing to a restart).
			if stored == nil {
				saveStored(ctx, s, key, result, errorTTL)
			}
		}
		return result, nil
	default:
		cachePut(s.cache, key, result, positiveTTL)
		saveStored(ctx, s, key, result, positiveTTL)
		return result, nil
	}
}

// cacheKey mints the cache key for an enrichment lookup: the asin key space when
// an asin is present (asin is preferred for the lookup), else the isbn one, else
// "" (nothing to look up).
func cacheKey(asin, isbn string) string {
	switch {
	case asin != "":
		return nsASIN.key(asin)
	case isbn != "":
		return nsISBN.key(isbn)
	default:
		return ""
	}
}

// lookup asks the upstream about one identifier (normalized; the asin preferred)
// for its work and the recording it matched. It neither reads nor writes the
// cache. On a nil error the work is non-nil with an id; a lookup without one is
// ErrNotFound.
func (s *Service) lookup(ctx context.Context, asin, isbn string) (*upstreamLookup, error) {
	l, err := s.client.lookup(ctx, asin, isbn)
	switch {
	case err != nil:
		return nil, err
	case l.Work == nil || l.Work.ID == "":
		return nil, ErrNotFound
	}
	return l, nil
}

// compose runs the uncached lookup -> work -> series fan-out. complete is false
// when the envelope is usable but a series rail fetch failed (the caller caches
// such a partial result only briefly). Its lookup is always a fresh upstream GET,
// never bounded by lookupSem and never recorded in the "l:" key space: the
// enrichment Enrich caches carries the work id already (WorkIDs reads it there),
// so a second copy would only shrink the room the lookup quota leaves to the
// console's own lookups.
func (s *Service) compose(ctx context.Context, asin, isbn string) (*Enrichment, bool, error) {
	lookup, err := s.lookup(ctx, asin, isbn)
	if err != nil {
		return nil, false, err
	}
	detail, err := s.client.work(ctx, lookup.Work.ID)
	if err != nil {
		// A work id handed back by lookup that then 404s is an upstream
		// inconsistency, not a clean "no match"; treat it as an error.
		if errors.Is(err, ErrNotFound) {
			return nil, false, errors.New("meta: lookup returned an unknown work id")
		}
		return nil, false, err
	}

	rails, complete := s.seriesRails(ctx, detail)
	env := &Enrichment{
		Matched:   true,
		Work:      s.toWork(detail),
		Recording: pickRecording(detail.Recordings, lookup.RecordingID),
		Series:    rails,
		WebURL:    s.workURL(detail.ID),
	}
	return env, complete, nil
}

// Work fetches a single work document by its metadata-site work id. It is the
// "catch me up on the previous book" path: the series rails in an Enrichment
// carry sibling work ids but no characters/recaps, so a client resolves one of
// those ids here to get the full expressive layer (characters, position-keyed
// recaps and the whole-book recap_summary) for that other book.
//
// It returns ErrNotFound when the work id is unknown upstream, and a non-nil,
// non-ErrNotFound error when the upstream is unreachable. Results (including
// "not found" and transport errors) are cached under a "w:" key space with the
// same TTL policy as Enrich, so a hot rail or a down upstream is not re-hit.
// Unlike Enrich this is a single upstream GET, already bounded by the client's
// per-request timeout, so it needs no extra fan-out deadline - but because the
// id is caller-chosen (not derived from a book this server holds) the uncached
// fetches are additionally bounded by maxConcurrentWorkFetches, and the work key
// space has its own cache quota (maxWorkEntries) so a flood of ids cannot evict
// the enrichment cache. A Store keeps the positive works only, for the same
// reason.
//
// The returned *MetaWork is shared with the cache and other callers - treat it
// as immutable; never modify it (or anything it points to) after the call.
func (s *Service) Work(ctx context.Context, id string) (*MetaWork, error) {
	id = strings.TrimSpace(id)
	// The handler already 400s a blank id; this is the service-level backstop, so
	// a future caller can't turn one into a bare works/ GET upstream.
	if id == "" {
		return nil, ErrNotFound
	}
	key := nsWork.key(id)
	// Same resolution as Enrich: a hit carries nil, ErrNotFound or the cached
	// transport error.
	if work, hit, err := cacheGet[MetaWork](s.cache, key); hit {
		return work, err
	}
	// Then the persistent store, as in Enrich. A work is stored only once it was
	// found, so a "no match" row is a stored work's later 404 (below).
	stored, expires, fresh := readStored[MetaWork](ctx, s, key)
	if fresh {
		cachePutUntil(s.cache, key, stored, expires)
		if stored == nil {
			return nil, ErrNotFound
		}
		return stored, nil
	}

	detail, err := s.fetchWork(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		// Held in memory only: the id is the caller's choice, so a persisted
		// miss would let any signed-in user grow the table (see Store). Except
		// over a stored answer for the id, which it replaces (no new row): left
		// alone, that row would bring the work upstream no longer has back as
		// the fallback of every later outage.
		s.cache.putMiss(key, notFoundTTL)
		if stored != nil {
			saveStored[MetaWork](ctx, s, key, nil, notFoundTTL)
		}
		return nil, ErrNotFound
	case err != nil:
		// Same discrimination as Enrich: only cache failures the UPSTREAM
		// caused. A failure from the CALLER's own cancelled context (players
		// abort in-flight fetches on navigation) must not poison this work with
		// 502s for the whole error TTL while upstream is healthy. And as in
		// Enrich, a persisted positive work outlives the outage.
		if ctx.Err() == nil {
			if stored != nil {
				cachePut(s.cache, key, stored, errorTTL)
				return stored, nil
			}
			s.cache.putError(key, err)
		}
		return nil, err
	}
	work := s.toWork(detail)
	cachePut(s.cache, key, work, positiveTTL)
	saveStored(ctx, s, key, work, positiveTTL)
	return work, nil
}

// fetchWork is one uncached works/{id} GET, bounded by workSem (only uncached
// fetches queue there, and a caller that goes away while queued never issues its
// GET at all). ErrNotFound for an unknown id - and for a wrong-shaped 200, see below.
func (s *Service) fetchWork(ctx context.Context, id string) (*upstreamWorkDetail, error) {
	select {
	case s.workSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Deferred so a panic can never leak a slot.
	defer func() { <-s.workSem }()
	detail, err := s.client.work(ctx, id)
	if err != nil {
		return nil, err
	}
	// A 200 that decodes to a zero-valued work is NOT a work. getJSON decodes
	// leniently, so any wrong-shaped 200 (upstream serving a different route for
	// this id - metaserve has a literal `works/latest` collection route that
	// outranks `works/{id}` in Go's ServeMux and returns `{"works":[...]}` - or a
	// proxy/error page with a JSON content type) lands here as an empty detail
	// with a nil error. Without this guard it would be cached POSITIVE for 24h and
	// served as a 200 carrying a blank work, breaking the client contract that any
	// failure reads as "unavailable" (it would render an empty card instead). Treat
	// it as the not-found it effectively is, mirroring lookup's guard against a
	// lookup without a work.
	if detail == nil || detail.ID == "" {
		return nil, ErrNotFound
	}
	return detail, nil
}

// toWork maps an upstream work document to the outward MetaWork shape. Shared
// by the enrichment composition and the work-id lookup so both expose exactly
// the same fields. A method only because the attribution's source_url is the
// work's page on this service's metadata site.
func (s *Service) toWork(detail *upstreamWorkDetail) *MetaWork {
	w := &MetaWork{
		ID:                   detail.ID,
		Title:                detail.Title,
		Subtitle:             detail.Subtitle,
		Authors:              toPersonRefs(detail.Authors),
		Language:             detail.Language,
		FirstPublished:       detail.FirstPublished,
		Description:          detail.Description,
		Characters:           toCharacters(detail.Characters),
		Recaps:               toRecaps(detail.Recaps),
		RecapSummary:         toRecapSummary(detail.RecapSummary),
		CommunityDescription: toCommunityDescription(detail.CommunityDescription),
	}
	if carriesCommunityContent(w) {
		w.Attribution = s.attribution(w.ID)
	}
	return w
}

// attribution builds the CC BY-SA credit for one work.
func (s *Service) attribution(workID string) *MetaAttribution {
	return &MetaAttribution{
		Credit:     attributionContributors,
		License:    attributionLicense,
		LicenseURL: attributionLicenseURL,
		SourceURL:  s.workURL(workID),
	}
}

// carriesCommunityContent reports whether w holds any of the CC BY-SA layer -
// the content its Attribution credits. HideSpoilers re-asks after gating, so a
// work whose every community entry was held back drops the credit too.
func carriesCommunityContent(w *MetaWork) bool {
	return len(w.Characters) > 0 || len(w.Recaps) > 0 || w.RecapSummary != nil || w.CommunityDescription != nil
}

// pickRecording returns the recording matching recordingID, falling back to the
// first recording when there is no match (or no id). Returns nil when the work
// has no recordings.
func pickRecording(recs []upstreamRecording, recordingID string) *MetaRecording {
	if len(recs) == 0 {
		return nil
	}
	chosen := &recs[0]
	if recordingID != "" {
		for i := range recs {
			if recs[i].ID == recordingID {
				chosen = &recs[i]
				break
			}
		}
	}
	return &MetaRecording{
		ID:           chosen.ID,
		Narrators:    toPersonRefs(chosen.Narrators),
		Abridged:     chosen.Abridged,
		RuntimeMin:   chosen.RuntimeMin,
		ReleaseDate:  chosen.ReleaseDate,
		Publisher:    chosen.Publisher,
		CoverURL:     chosen.CoverURL,
		ChapterCount: chosen.ChapterCount,
	}
}

// seriesRails builds one rail per ordering FAMILY the work belongs to (capped
// at maxSeriesRails families). A family is keyed by a ref's ordering_of, or by
// its own id when that is empty, so a pre-v7 metaserve - which sends no
// ordering fields - makes every series its own family and every rail exactly
// what it was before orderings existed. The one exception is a work listed at
// two positions of ONE series (series_works does not forbid it): that used to
// be two rails of the same series and is now one, at the first position. The family's MAIN view is chosen by
// familyMains: the primary whenever the work is in it, else the variant that
// holds it (a variant-only work keeps its variant, the only order that places
// it).
//
// A per-series fetch failure is non-fatal: a failed main view skips the rail,
// and a failed alternate ships the rail without that alternate, so the rest of
// the enrichment (progressive enhancement) still returns - but either is
// reported via complete=false so the caller caches the partial envelope only
// briefly instead of hiding a rail or an order for the whole positive TTL.
func (s *Service) seriesRails(ctx context.Context, detail *upstreamWorkDetail) (rails []MetaSeries, complete bool) {
	// The work's own position in each series it belongs to, by series id. An
	// alternate's position is read from here: the work detail is the one place
	// that states where THIS work sits, and a series the work is absent from
	// has no entry, which is exactly the empty position the envelope promises.
	positions := make(map[string]string, len(detail.Series))
	for _, ref := range detail.Series {
		if _, seen := positions[ref.ID]; !seen {
			positions[ref.ID] = ref.Position
		}
	}

	// Bound ATTEMPTS up front, not successes: with a failing series endpoint and
	// odd/hostile data (dozens of series refs on one work), a success-counted
	// loop would issue one 5s-timeout GET per ref. The cap counts families, so
	// grouping happens first.
	mains := familyMains(detail.Series)
	if len(mains) > maxSeriesRails {
		mains = mains[:maxSeriesRails]
	}
	complete = true
	var out []MetaSeries
	// Every MAIN view is fetched before any alternate. The fan-out is sequential
	// under one composeTimeout budget, so interleaving rail 1's alternates ahead
	// of rail 2's main would let a slow-but-alive upstream spend the budget on
	// optional views and drop whole rails - the thing a shipped player renders.
	// Alternates go last; a deadline that fires among them costs only alternates.
	families := make([][]upstreamSeriesOrdering, 0, len(mains))
	for _, ref := range mains {
		sd, err := s.client.series(ctx, ref.ID)
		if err != nil || sd == nil {
			complete = false
			continue
		}
		out = append(out, MetaSeries{
			ID:       ref.ID,
			Name:     ref.Name,
			Position: ref.Position,
			Works:    s.railWorks(sd),
			Ordering: sd.Ordering,
			// The ref's ordering_of, not the detail's: it is what grouped this
			// family, so a client's `ordering_of || id` key agrees with the
			// server's collapse.
			OrderingOf: ref.OrderingOf,
		})
		families = append(families, sd.Orderings)
	}
	for i := range out {
		alts, ok := s.orderingAlternates(ctx, out[i].ID, families[i], positions)
		if !ok {
			complete = false
		}
		out[i].Orderings = alts
	}
	return out, complete
}

// familyMains returns the MAIN ref of each ordering family in refs, families
// in order of first appearance. Within a family the main view is the first ref
// whose ordering_of is empty - the primary - else the family's first ref (a
// variant-only work keeps its variant). The rule reads ordering_of rather than
// trusting metaserve to list memberships primary-first: a variant leading the
// rail would hand a shipped player the chronological order's earlier books as
// "previous" (the reading-order spoiler), and nothing here could see it happen.
// audiosilo-sidecars' readingSeries chooses by the same rule.
func familyMains(refs []upstreamSeriesRef) []upstreamSeriesRef {
	index := make(map[string]int, len(refs)) // family key -> slot in mains
	var mains []upstreamSeriesRef
	for _, ref := range refs {
		key := ref.OrderingOf
		if key == "" {
			key = ref.ID
		}
		i, seen := index[key]
		if !seen {
			index[key] = len(mains)
			mains = append(mains, ref)
			continue
		}
		if ref.OrderingOf == "" && mains[i].OrderingOf != "" {
			mains[i] = ref
		}
	}
	return mains
}

// orderingAlternates fetches the family's other reading orders - every member
// of the main view's `orderings` except the main view itself, in metaserve's
// family order, bounded to maxOrderingAlternates attempts. The family is read
// from the main view's detail rather than from the work's own refs because a
// work need not sit in every order of its family, and an order that omits it is
// still a view the reader may want. ok is false when an alternate's fetch
// failed; that alternate is left out and the others still ship.
func (s *Service) orderingAlternates(ctx context.Context, mainID string, family []upstreamSeriesOrdering, positions map[string]string) (alts []MetaSeriesOrdering, ok bool) {
	ok = true
	attempts := 0
	// A member listed twice is one view: fetching it again would spend an
	// attempt on a duplicate alternate (and ship it twice).
	seen := map[string]bool{mainID: true}
	for _, member := range family {
		if member.ID == "" || seen[member.ID] {
			continue
		}
		if attempts == maxOrderingAlternates {
			break
		}
		seen[member.ID] = true
		attempts++
		sd, err := s.client.series(ctx, member.ID)
		if err != nil || sd == nil {
			ok = false
			continue
		}
		alts = append(alts, MetaSeriesOrdering{
			ID:       member.ID,
			Name:     member.Name,
			Ordering: member.Ordering,
			// The family listing states no ordering_of; the alternate's own
			// detail does, and it is set on every variant (empty on the primary).
			OrderingOf: sd.OrderingOf,
			Position:   positions[member.ID],
			Works:      s.railWorks(sd),
		})
	}
	return alts, ok
}

// railWorks builds the ordered work entries of one series view, each carrying
// its own web_url. Shared by a rail's main view and its alternates so both
// expose exactly the same MetaSeriesWork shape.
func (s *Service) railWorks(sd *upstreamSeriesDetail) []MetaSeriesWork {
	works := make([]MetaSeriesWork, 0, len(sd.Works))
	for _, entry := range sd.Works {
		if entry.Work == nil {
			continue
		}
		works = append(works, MetaSeriesWork{
			ID:       entry.Work.ID,
			Title:    entry.Work.Title,
			Position: entry.Position,
			Authors:  toPersonRefs(entry.Work.Authors),
			CoverURL: deref(entry.Work.CoverURL),
			WebURL:   s.workURL(entry.Work.ID),
		})
	}
	return works
}

// workURL builds the metadata site URL for a work id.
func (s *Service) workURL(id string) string {
	return s.baseURL + "/work?id=" + url.QueryEscape(id)
}

func toPersonRefs(in []upstreamPersonRef) []MetaPersonRef {
	out := make([]MetaPersonRef, 0, len(in))
	for _, p := range in {
		out = append(out, MetaPersonRef(p))
	}
	return out
}

// toCharacters maps the upstream character sidecar to the outward envelope,
// preserving upstream order. Returns nil (omitted) when there are none.
func toCharacters(in []upstreamCharacter) []MetaCharacter {
	if len(in) == 0 {
		return nil
	}
	out := make([]MetaCharacter, 0, len(in))
	for _, c := range in {
		out = append(out, MetaCharacter{
			ID:          c.ID,
			Name:        c.Name,
			Aliases:     c.Aliases,
			Role:        c.Role,
			Reveal:      MetaPosition(c.Reveal),
			Description: c.Description,
		})
	}
	return out
}

// toRecaps maps the upstream recap sidecar to the outward envelope, preserving
// upstream (position) order. Returns nil (omitted) when there are none.
func toRecaps(in []upstreamRecap) []MetaRecap {
	if len(in) == 0 {
		return nil
	}
	out := make([]MetaRecap, 0, len(in))
	for _, r := range in {
		out = append(out, MetaRecap{
			Through: MetaPosition(r.Through),
			Scope:   r.Scope,
			Text:    r.Text,
		})
	}
	return out
}

// toRecapSummary maps the upstream whole-book summary to the outward envelope.
// Returns nil (omitted) when upstream has none, or when it is present but
// entirely empty - an all-blank object would render as an empty catch-up card.
func toRecapSummary(in *upstreamRecapSummary) *MetaRecapSummary {
	if in == nil || (in.InShort == "" && in.Ending == "") {
		return nil
	}
	return &MetaRecapSummary{InShort: in.InShort, Ending: in.Ending}
}

// toCommunityDescription maps the upstream CC BY-SA description. Returns nil
// (omitted) when upstream has none, or - defensively, since metaserve never
// sends one - when its text is blank, which would render as an empty block
// under a license credit.
func toCommunityDescription(in *upstreamCommunityDescription) *MetaCommunityDescription {
	if in == nil || strings.TrimSpace(in.Text) == "" {
		return nil
	}
	return &MetaCommunityDescription{Text: in.Text, License: in.License}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
