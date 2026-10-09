// Package meta resolves a book's asin/isbn against the community metadata API
// (metaserve, meta.audiosilo.app) and composes a server-side enrichment envelope
// for the player. The lookup is server-side by design: one cached seam, and a
// self-hosted admin can disable all outbound calls with a single config key
// (internal/config.MetadataConfig). This package holds the business logic; the
// api package stays transport-only.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrNotFound is returned when the upstream lookup has no match for the given
// asin/isbn (a 404 from metaserve's /lookup). The api handler maps it to a
// 200 {"matched": false} response.
var ErrNotFound = errors.New("meta: not found")

// errLocalNotFound is a 404 the local copy answered (mirror mode, the
// fallbackTransport's localAnswerHeader): ErrNotFound to every caller
// (errors.Is), and to keepStored the copy's own "no match", which never replaces
// a stored answer the way the remote service's does.
var errLocalNotFound = fmt.Errorf("%w (the local copy)", ErrNotFound)

// clientTimeout bounds a single upstream HTTP request. Kept short so a slow or
// unreachable metadata service degrades to a fast 502 rather than tying up the
// per-request timeout budget.
const clientTimeout = 5 * time.Second

// client is a thin read-only HTTP client for the metaserve JSON API. It GETs and
// decodes the small set of endpoints the enrichment composition needs.
type client struct {
	baseURL string // no trailing slash; API is served under <baseURL>/api/v1
	http    *http.Client
}

// newClient builds a client for the given metaserve base URL. baseURL is
// expected already trimmed of a trailing slash (NewService does the trim).
func newClient(baseURL string) *client {
	return &client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: clientTimeout},
	}
}

// getJSON fetches path (relative to the API root) and decodes the JSON body into
// out. A 404 becomes ErrNotFound (errLocalNotFound when the local copy answered
// it); any other non-2xx (or transport failure) is a plain error the caller
// treats as an upstream outage.
func (c *client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Drain a little so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		if resp.Header.Get(localAnswerHeader) != "" {
			return errLocalNotFound
		}
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{code: resp.StatusCode, path: path}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("meta: decode %s: %w", path, err)
	}
	return nil
}

// statusError is an upstream answer outside 2xx other than a 404.
type statusError struct {
	code int
	path string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("meta: upstream status %d for %s", e.code, e.path)
}

// lookup resolves an asin (preferred) or isbn to a work id + recording id via
// GET /api/v1/lookup. Returns ErrNotFound when there is no match.
func (c *client) lookup(ctx context.Context, asin, isbn string) (*upstreamLookup, error) {
	q := url.Values{}
	switch {
	case asin != "":
		q.Set("asin", asin)
	case isbn != "":
		q.Set("isbn", isbn)
	default:
		return nil, ErrNotFound
	}
	var out upstreamLookup
	if err := c.getJSON(ctx, "/api/v1/lookup?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// work fetches the full work document via GET /api/v1/works/{id}.
func (c *client) work(ctx context.Context, id string) (*upstreamWorkDetail, error) {
	var out upstreamWorkDetail
	if err := c.getJSON(ctx, "/api/v1/works/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// searchWorks runs a ranked full-text work search via GET /api/v1/works/search.
func (c *client) searchWorks(ctx context.Context, q string, limit int) (*upstreamSearch, error) {
	v := url.Values{}
	v.Set("q", q)
	v.Set("limit", strconv.Itoa(limit))
	var out upstreamSearch
	if err := c.getJSON(ctx, "/api/v1/works/search?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// errNoMatchResults is a works/match 200 that is not a match answer: no
// "results" member at all. A metaserve cannot send one, so it is something else
// answering for it, and the caller falls back as it does for a 404.
var errNoMatchResults = errors.New("meta: works/match answered without results")

// matchWorks runs metaserve's structured match via GET /api/v1/works/match. An
// older metaserve has no such route and answers 404 (ErrNotFound, through
// works/{id} reading "match" as an id); see routeMissing.
func (c *client) matchWorks(ctx context.Context, v url.Values) ([]upstreamMatchResult, error) {
	var out struct {
		// Raw, so a body WITHOUT the member (nil) is told apart from an empty
		// answer, "[]" or "null" alike.
		Results json.RawMessage `json:"results"`
	}
	if err := c.getJSON(ctx, "/api/v1/works/match?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	if out.Results == nil {
		return nil, errNoMatchResults
	}
	var results []upstreamMatchResult
	if err := json.Unmarshal(out.Results, &results); err != nil {
		return nil, fmt.Errorf("meta: decode works/match results: %w", err)
	}
	return results, nil
}

// series fetches an ordered series rail via GET /api/v1/series/{id}.
func (c *client) series(ctx context.Context, id string) (*upstreamSeriesDetail, error) {
	var out upstreamSeriesDetail
	if err := c.getJSON(ctx, "/api/v1/series/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// recordingChapters fetches a recording's chapter list via GET
// /api/v1/works/{id}/recordings/{rid}/chapters. metaserve answers an unknown work
// or recording with an empty list, and an older metaserve without the route with
// a 404 (ErrNotFound).
func (c *client) recordingChapters(ctx context.Context, workID, recordingID string) ([]Chapter, error) {
	var out struct {
		Chapters []Chapter `json:"chapters"`
	}
	if err := c.getJSON(ctx, "/api/v1/works/"+url.PathEscape(workID)+"/recordings/"+url.PathEscape(recordingID)+"/chapters", &out); err != nil {
		return nil, err
	}
	return out.Chapters, nil
}

// ---- upstream shapes (mirror metaserve's internal/serve JSON exactly) --------

type upstreamPersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// upstreamSeriesRef is one of a work's series memberships. OrderingOf is set
// (artifact schema_version 7) only when the series is a VARIANT reading order -
// a chronological or recommended listing - of another series, and names that
// primary. A pre-v7 metaserve never sends it, which is what keeps the rail
// grouping in seriesRails exactly today's behaviour against an older upstream.
type upstreamSeriesRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Position   string `json:"position"`
	OrderingOf string `json:"ordering_of,omitempty"`
}

type upstreamWorkCard struct {
	ID       string              `json:"id"`
	Title    string              `json:"title"`
	Authors  []upstreamPersonRef `json:"authors"`
	CoverURL *string             `json:"cover_url"`
}

type upstreamLookup struct {
	Work        *upstreamWorkCard `json:"work"`
	RecordingID string            `json:"recording_id"`
}

type upstreamRecording struct {
	ID          string              `json:"id"`
	Narrators   []upstreamPersonRef `json:"narrators"`
	Abridged    bool                `json:"abridged"`
	RuntimeMin  int                 `json:"runtime_min"`
	ReleaseDate string              `json:"release_date"`
	Publisher   string              `json:"publisher"`
	CoverURL    string              `json:"cover_url"`
	// ChapterCount is the recording's chapter count, 0 when upstream does not
	// know it.
	ChapterCount int            `json:"chapter_count"`
	ASIN         []upstreamASIN `json:"asin"`
	ISBN         []string       `json:"isbn"`
}

// upstreamASIN is one of a recording's ASINs; Audible ASINs are per marketplace.
type upstreamASIN struct {
	Region string `json:"region"`
	ASIN   string `json:"asin"`
}

// upstreamSearch is GET works/search: ranked work hits (the card fields are not
// needed here; each hit's full document is fetched by id).
type upstreamSearch struct {
	Results []struct {
		ID       string  `json:"id"`
		CoverURL *string `json:"cover_url"`
	} `json:"results"`
}

// upstreamMatchResult is one GET works/match result, best first: a work card
// (only the id and cover are needed; the full document is fetched by id) with
// its score, reasons and the recording an identifier named.
type upstreamMatchResult struct {
	ID          string        `json:"id"`
	CoverURL    *string       `json:"cover_url"`
	Score       int           `json:"score"`
	RecordingID string        `json:"recording_id"`
	Reasons     *MatchReasons `json:"reasons"`
}

type upstreamPosition struct {
	Chapter int `json:"chapter"`
}

type upstreamCharacter struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Aliases     []string         `json:"aliases"`
	Role        string           `json:"role"`
	Reveal      upstreamPosition `json:"reveal"`
	Description string           `json:"description"`
}

type upstreamRecap struct {
	Through upstreamPosition `json:"through"`
	Scope   string           `json:"scope"`
	Text    string           `json:"text"`
}

// upstreamRecapSummary is metaserve's per-work whole-book refresher: a short
// "in short" paragraph plus a plain statement of how the book ends. Both fields
// are optional upstream, and the object itself is omitted for works that have no
// summary sidecar (so a nil pointer is the normal case).
type upstreamRecapSummary struct {
	InShort string `json:"in_short"`
	Ending  string `json:"ending"`
}

// upstreamCommunityDescription is metaserve's CC BY-SA, spoiler-free work
// description (the works-community layer), named apart from the CC0 core
// `description` because the two carry different licenses. The object is omitted
// for a work without one, and its text is never empty when it is present.
type upstreamCommunityDescription struct {
	Text    string `json:"text"`
	License string `json:"license"`
}

type upstreamWorkDetail struct {
	ID             string                `json:"id"`
	Title          string                `json:"title"`
	Subtitle       string                `json:"subtitle"`
	Authors        []upstreamPersonRef   `json:"authors"`
	Language       string                `json:"language"`
	FirstPublished string                `json:"first_published"`
	Description    string                `json:"description"`
	Series         []upstreamSeriesRef   `json:"series"`
	Recordings     []upstreamRecording   `json:"recordings"`
	Characters     []upstreamCharacter   `json:"characters"`
	Recaps         []upstreamRecap       `json:"recaps"`
	RecapSummary   *upstreamRecapSummary `json:"recap_summary"`
	// CommunityDescription is the CC BY-SA description (see
	// upstreamCommunityDescription); nil for most works.
	CommunityDescription *upstreamCommunityDescription `json:"community_description"`
}

type upstreamSeriesEntry struct {
	Position string            `json:"position"`
	Work     *upstreamWorkCard `json:"work"`
}

// upstreamSeriesOrdering is one member of an ordering FAMILY (a primary series
// plus every variant whose ordering_of names it). Ordering is the reading order
// that series states (publication / chronological / recommended), omitted when
// unstated.
type upstreamSeriesOrdering struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Ordering string `json:"ordering,omitempty"`
}

// upstreamSeriesDetail is GET series/{id}. Ordering, OrderingOf and Orderings
// arrive with artifact schema_version 7 and are absent before it. Orderings is
// the whole family - primary first, then the variants by id - served
// identically on every member, and omitted when the series has no variant.
type upstreamSeriesDetail struct {
	ID         string                   `json:"id"`
	Name       string                   `json:"name"`
	Ordering   string                   `json:"ordering,omitempty"`
	OrderingOf string                   `json:"ordering_of,omitempty"`
	Authors    []upstreamPersonRef      `json:"authors"`
	Works      []upstreamSeriesEntry    `json:"works"`
	Orderings  []upstreamSeriesOrdering `json:"orderings,omitempty"`
}
