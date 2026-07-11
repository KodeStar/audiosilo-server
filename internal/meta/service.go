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
// per lookup regardless of odd data.
const maxSeriesRails = 3

// MetaPersonRef is the {id,name} shape for an author or narrator.
type MetaPersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
	client  *Client
	baseURL string // metaserve base URL (no trailing slash) for building web_url
	cache   *cache
	now     func() time.Time
}

// NewService builds a Service for the given metaserve base URL. now may be nil
// (defaults to time.Now); it is injectable so tests can drive cache TTLs.
func NewService(baseURL string, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	base := strings.TrimRight(baseURL, "/")
	return &Service{
		client:  NewClient(base),
		baseURL: base,
		cache:   newCache(now),
		now:     now,
	}
}

// Enrich resolves a book's asin (preferred) or isbn to a composed enrichment.
// It returns ErrNotFound when there is no match, and a non-nil, non-ErrNotFound
// error when the upstream is unreachable. Results (including "not found" and
// transport errors) are cached so a hot path or a down upstream is not re-hit.
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

	result, err := s.compose(ctx, asin, isbn)
	switch {
	case errors.Is(err, ErrNotFound):
		s.cache.putResult(key, nil, notFoundTTL)
		return nil, ErrNotFound
	case err != nil:
		// Only cache failures the UPSTREAM caused. When the caller's own context
		// is done (the client aborted the request or its deadline passed -
		// routine: the player cancels in-flight fetches on navigation), caching
		// the resulting error would poison this book's enrichment with 502s for
		// the whole error TTL while upstream is perfectly healthy. ctx.Err()
		// cleanly discriminates the two: it is nil for a genuine upstream failure,
		// including the client's own 5s timeout firing under a live caller.
		if ctx.Err() == nil {
			s.cache.putError(key, err)
		}
		return nil, err
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

// compose runs the uncached lookup -> work -> series fan-out.
func (s *Service) compose(ctx context.Context, asin, isbn string) (*Enrichment, error) {
	lookup, err := s.client.Lookup(ctx, asin, isbn)
	if err != nil {
		return nil, err
	}
	if lookup.Work == nil {
		return nil, ErrNotFound
	}
	detail, err := s.client.Work(ctx, lookup.Work.ID)
	if err != nil {
		// A work id handed back by lookup that then 404s is an upstream
		// inconsistency, not a clean "no match"; treat it as an error.
		if errors.Is(err, ErrNotFound) {
			return nil, errors.New("meta: lookup returned an unknown work id")
		}
		return nil, err
	}

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
		},
		Recording: pickRecording(detail.Recordings, lookup.RecordingID),
		Series:    s.seriesRails(ctx, detail),
		WebURL:    s.workURL(detail.ID),
	}
	return env, nil
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
// returns. The work's own position in the series comes from the work detail.
func (s *Service) seriesRails(ctx context.Context, detail *upstreamWorkDetail) []MetaSeries {
	var out []MetaSeries
	for _, ref := range detail.Series {
		if len(out) >= maxSeriesRails {
			break
		}
		sd, err := s.client.Series(ctx, ref.ID)
		if err != nil || sd == nil {
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
	return out
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
