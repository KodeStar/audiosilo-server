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
	"time"
)

// ErrNotFound is returned when the upstream lookup has no match for the given
// asin/isbn (a 404 from metaserve's /lookup). The api handler maps it to a
// 200 {"matched": false} response.
var ErrNotFound = errors.New("meta: not found")

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
// out. A 404 becomes ErrNotFound; any other non-2xx (or transport failure) is a
// plain error the caller treats as an upstream outage.
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
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("meta: upstream status %d for %s", resp.StatusCode, path)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("meta: decode %s: %w", path, err)
	}
	return nil
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

// series fetches an ordered series rail via GET /api/v1/series/{id}.
func (c *client) series(ctx context.Context, id string) (*upstreamSeriesDetail, error) {
	var out upstreamSeriesDetail
	if err := c.getJSON(ctx, "/api/v1/series/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- upstream shapes (mirror metaserve's internal/serve JSON exactly) --------

type upstreamPersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type upstreamSeriesRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position string `json:"position"`
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
}

type upstreamSeriesEntry struct {
	Position string            `json:"position"`
	Work     *upstreamWorkCard `json:"work"`
}

type upstreamSeriesDetail struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Authors []upstreamPersonRef   `json:"authors"`
	Works   []upstreamSeriesEntry `json:"works"`
}
