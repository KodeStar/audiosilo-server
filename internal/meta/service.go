package meta

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// maxSeriesRails caps how many series a work's enrichment fetches. A work is
// rarely in more than one or two series; the cap bounds the fan-out to metaserve
// per lookup regardless of odd data. It bounds ATTEMPTS, not successes, so
// hostile upstream data (a work carrying dozens of series refs) with a failing
// series endpoint can never issue more than this many series requests.
const maxSeriesRails = 3

// composeTimeout bounds one full compose fan-out (lookup + work + up to
// maxSeriesRails series calls). Each call already has the client's 5s timeout,
// but sequentially those could sum to ~25s against the API's 30s request budget;
// this keeps the whole composition comfortably under it. When THIS deadline
// fires while the caller is still live, the parent ctx.Err() stays nil, so the
// failure IS cached as a transport error for errorTTL - exactly the protective
// behavior we want against a degraded-but-alive upstream (it is not re-hammered
// on every cold book).
const composeTimeout = 15 * time.Second

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

// MetaWork is the abstract book in an enrichment envelope.
type MetaWork struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Subtitle       string          `json:"subtitle,omitempty"`
	Authors        []MetaPersonRef `json:"authors"`
	Language       string          `json:"language"`
	FirstPublished string          `json:"first_published,omitempty"`
	Description    string          `json:"description,omitempty"`
	Characters     []MetaCharacter `json:"characters,omitempty"`
	Recaps         []MetaRecap     `json:"recaps,omitempty"`
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
}

// MetaSeries is a full ordered series rail, including the current work.
type MetaSeries struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Position string           `json:"position"` // the current work's position in this series
	Works    []MetaSeriesWork `json:"works"`
}

// Enrichment is the composed envelope returned on a match. Matched is always true
// here; the api handler emits {"matched": false} for the no-match / no-ids cases.
type Enrichment struct {
	Matched   bool           `json:"matched"`
	Work      *MetaWork      `json:"work,omitempty"`
	Recording *MetaRecording `json:"recording,omitempty"`
	Series    []MetaSeries   `json:"series,omitempty"`
	WebURL    string         `json:"web_url"`
}

// Service composes book enrichments from the metadata API, with a bounded TTL
// cache in front so a repeated lookup (and a down upstream) is cheap. A nil
// *Service means the feature is off; callers must guard on that.
type Service struct {
	client  *client
	baseURL string // metaserve base URL (no trailing slash) for building web_url
	cache   *cache
	// composeBudget bounds one full compose fan-out. Defaults to composeTimeout;
	// a field only so tests can shrink it.
	composeBudget time.Duration
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
	}
}

// Enrich resolves a book's asin (preferred) or isbn to a composed enrichment.
// It returns ErrNotFound when there is no match, and a non-nil, non-ErrNotFound
// error when the upstream is unreachable. Results (including "not found" and
// transport errors) are cached so a hot path or a down upstream is not re-hit.
//
// The returned *Enrichment is shared with the cache and other callers - treat
// it as immutable; never modify it (or anything it points to) after the call.
func (s *Service) Enrich(ctx context.Context, asin, isbn string) (*Enrichment, error) {
	asin = strings.TrimSpace(asin)
	isbn = strings.TrimSpace(isbn)
	key := cacheKey(asin, isbn)
	if key == "" {
		return nil, ErrNotFound
	}
	if e, ok := s.cache.get(key); ok {
		switch {
		case e.err != nil:
			return nil, e.err
		case e.result == nil:
			return nil, ErrNotFound
		default:
			return e.result, nil
		}
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
		s.cache.putResult(key, nil, notFoundTTL)
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
			s.cache.putError(key, err)
		}
		return nil, err
	case !complete:
		// A usable envelope, but at least one series rail failed transiently.
		// Caching it for the full positive TTL would hide "more in this series"
		// for a day on a blip, so hold it only briefly (errorTTL) and retry
		// soon. And if the caller's context is done, the missing rails were
		// caused by the CALLER's cancellation mid-fan-out - cache nothing at
		// all, same reasoning as the error branch above.
		if ctx.Err() == nil {
			s.cache.putResult(key, result, errorTTL)
		}
		return result, nil
	default:
		s.cache.putResult(key, result, positiveTTL)
		return result, nil
	}
}

// cacheKey is "a:"+asin when an asin is present (asin is preferred for the
// lookup), else "i:"+isbn, else "" (nothing to look up).
func cacheKey(asin, isbn string) string {
	switch {
	case asin != "":
		return "a:" + asin
	case isbn != "":
		return "i:" + isbn
	default:
		return ""
	}
}

// compose runs the uncached lookup -> work -> series fan-out. complete is false
// when the envelope is usable but a series rail fetch failed (the caller caches
// such a partial result only briefly).
func (s *Service) compose(ctx context.Context, asin, isbn string) (*Enrichment, bool, error) {
	lookup, err := s.client.lookup(ctx, asin, isbn)
	if err != nil {
		return nil, false, err
	}
	if lookup.Work == nil {
		return nil, false, ErrNotFound
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
		Matched: true,
		Work: &MetaWork{
			ID:             detail.ID,
			Title:          detail.Title,
			Subtitle:       detail.Subtitle,
			Authors:        toPersonRefs(detail.Authors),
			Language:       detail.Language,
			FirstPublished: detail.FirstPublished,
			Description:    detail.Description,
			Characters:     toCharacters(detail.Characters),
			Recaps:         toRecaps(detail.Recaps),
		},
		Recording: pickRecording(detail.Recordings, lookup.RecordingID),
		Series:    rails,
		WebURL:    s.workURL(detail.ID),
	}
	return env, complete, nil
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
		ID:          chosen.ID,
		Narrators:   toPersonRefs(chosen.Narrators),
		Abridged:    chosen.Abridged,
		RuntimeMin:  chosen.RuntimeMin,
		ReleaseDate: chosen.ReleaseDate,
		Publisher:   chosen.Publisher,
		CoverURL:    chosen.CoverURL,
	}
}

// seriesRails fetches each series the work belongs to (capped) and builds the
// full ordered rail for each. A per-series fetch failure is non-fatal: that rail
// is skipped so the rest of the enrichment (progressive enhancement) still
// returns - but it is reported via complete=false so the caller caches the
// partial envelope only briefly instead of hiding the rail for the whole
// positive TTL. The work's own position in the series comes from the work
// detail.
func (s *Service) seriesRails(ctx context.Context, detail *upstreamWorkDetail) (rails []MetaSeries, complete bool) {
	// Bound ATTEMPTS up front, not successes: with a failing series endpoint and
	// odd/hostile data (dozens of series refs on one work), a success-counted
	// loop would issue one 5s-timeout GET per ref.
	refs := detail.Series
	if len(refs) > maxSeriesRails {
		refs = refs[:maxSeriesRails]
	}
	complete = true
	var out []MetaSeries
	for _, ref := range refs {
		sd, err := s.client.series(ctx, ref.ID)
		if err != nil || sd == nil {
			complete = false
			continue
		}
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
		out = append(out, MetaSeries{
			ID:       ref.ID,
			Name:     ref.Name,
			Position: ref.Position,
			Works:    works,
		})
	}
	return out, complete
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
