package meta

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The community facts the upcoming releases job (internal/upcoming) reads: a
// work's series, a series' members with their release dates, and metaserve's
// watch feed (which series have news). None is cached here: the job keeps what
// it read in its own tables and asks again on its own schedule. In mirror mode
// they go through the same client, so the local copy answers them
// (fallbackTransport), as every other question.

// IDKey is the key one book's identifiers are resolved by (an asin, else an
// isbn, normalized): a book whose key changes is resolved again. "" for none.
func IDKey(asin, isbn string) string {
	return cacheKey(match.NormalizeASIN(asin), match.NormalizeISBN(isbn))
}

// WorkURL is the community site's page for a work, as every web_url names it.
func (s *Service) WorkURL(id string) string { return s.workURL(id) }

// SeriesRef is one of a work's series: its id and name, and the primary series
// it is a variant reading order of (OrderingOf, "" for a primary).
type SeriesRef struct {
	ID         string
	Name       string
	OrderingOf string
}

// WorkSeries is the series work id is in (works/{id}, uncached, bounded like
// every uncached work fetch). ErrNotFound for an unknown work.
func (s *Service) WorkSeries(ctx context.Context, id string) ([]SeriesRef, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}
	detail, err := s.fetchWork(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]SeriesRef, 0, len(detail.Series))
	for _, r := range detail.Series {
		if r.ID != "" {
			out = append(out, SeriesRef{ID: r.ID, Name: r.Name, OrderingOf: r.OrderingOf})
		}
	}
	return out, nil
}

// SeriesMember is one entry of a community series.
type SeriesMember struct {
	WorkID   string
	Position string
	Title    string
	Authors  []MetaPersonRef
	CoverURL string
	// ReleaseDate is the work's first release at the precision stated, "" when
	// unknown; AddedAt when the catalogue learned of it, "" when unknown.
	ReleaseDate string
	AddedAt     string
}

// SeriesRecord is a community series with its members in series order.
type SeriesRecord struct {
	ID      string
	Name    string
	Members []SeriesMember
}

// Series fetches series/{id} (uncached). ErrNotFound for an unknown series,
// and for a 200 that is no series (no id), as fetchWork reads one.
func (s *Service) Series(ctx context.Context, id string) (*SeriesRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}
	select {
	case s.workSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.workSem }()
	sd, err := s.client.series(ctx, id)
	if err != nil {
		return nil, err
	}
	if sd == nil || sd.ID == "" {
		return nil, ErrNotFound
	}
	rec := &SeriesRecord{ID: sd.ID, Name: sd.Name}
	for _, e := range sd.Works {
		if e.Work == nil || e.Work.ID == "" {
			continue
		}
		rec.Members = append(rec.Members, SeriesMember{
			WorkID:      e.Work.ID,
			Position:    e.Position,
			Title:       e.Work.Title,
			Authors:     toPersonRefs(e.Work.Authors),
			CoverURL:    deref(e.Work.CoverURL),
			ReleaseDate: e.Work.ReleaseDate,
			AddedAt:     deref(e.Work.AddedAt),
		})
	}
	return rec, nil
}

// MaxFeedSeries is how many series one watch feed request names (metaserve
// takes up to 200; fewer keep the URL short and the 200-item cap far off).
const MaxFeedSeries = 50

// MaxFeedItems is metaserve's cap on one watch feed's items: a feed that
// carries this many may have dropped some (the oldest).
const MaxFeedItems = 200

// FeedItem is one watch feed item: the work it is about, its state
// ("preorder" or "released", from the item's id), its release date
// (YYYY-MM-DD; "" for none, and for a work whose date is unknown: Undated) and
// its title ("<series> #<position>: <title>").
type FeedItem struct {
	WorkID string
	State  string
	Date   string
	Title  string
	// Undated: the work states no usable release date, and the feed lists it
	// for its addition to the catalogue (dated by that, not by a release).
	Undated bool
}

// feedUndated is the tag metaserve's watch feed gives an item whose work
// states no usable release date (its date_published is then the work's
// added_at).
const feedUndated = "date unknown"

// ErrInvalidSlug is WatchFeed's answer for a series id metaserve would refuse.
var ErrInvalidSlug = errors.New("meta: not a series slug")

// WatchFeed reads metaserve's JSON watch feed for series (at most
// MaxFeedSeries, each a valid slug) over a window of days: the works that are
// news in them (preorders, and releases or additions within the window). An
// item whose work id can't be read is skipped (WorkIDFromFeedItem).
func (s *Service) WatchFeed(ctx context.Context, series []string, window int) ([]FeedItem, error) {
	if len(series) == 0 {
		return nil, nil
	}
	if len(series) > MaxFeedSeries {
		return nil, errors.New("meta: too many series for one watch feed")
	}
	for _, id := range series {
		if !model.ValidSlug(id) {
			return nil, ErrInvalidSlug
		}
	}
	v := url.Values{}
	v.Set("s", strings.Join(series, ","))
	if window > 0 {
		v.Set("window", strconv.Itoa(window))
	}
	var feed struct {
		Items []struct {
			ID            string   `json:"id"`
			URL           string   `json:"url"`
			Title         string   `json:"title"`
			DatePublished string   `json:"date_published"`
			Tags          []string `json:"tags"`
		} `json:"items"`
	}
	if err := s.client.getJSON(ctx, "/api/v1/watch/feed.json?"+v.Encode(), &feed); err != nil {
		return nil, err
	}
	out := make([]FeedItem, 0, len(feed.Items))
	for _, it := range feed.Items {
		work, state := WorkIDFromFeedItem(it.ID, it.URL)
		if work == "" {
			continue
		}
		undated := slices.Contains(it.Tags, feedUndated)
		date := ""
		if len(it.DatePublished) >= 10 && !undated {
			date = it.DatePublished[:10]
		}
		out = append(out, FeedItem{WorkID: work, State: state, Date: date, Title: it.Title, Undated: undated})
	}
	return out, nil
}

// feedTagPrefix is the watch feed's item id prefix: the work id and the state
// follow ("tag:meta.audiosilo.app,2026:work/<id>/<state>").
const feedTagPrefix = "tag:meta.audiosilo.app,2026:work/"

// WorkIDFromFeedItem reads the work id (and the state, "" when unknown) a watch
// feed item is about: from its tag id, else from its url's /works/<id> path
// (the way the community site's watch badge reads it). "" when neither names
// a work: a malformed item is skipped, never guessed at.
func WorkIDFromFeedItem(id, link string) (work, state string) {
	if rest, ok := strings.CutPrefix(id, feedTagPrefix); ok {
		if w, st, ok := strings.Cut(rest, "/"); ok && validWork(w) && (st == "preorder" || st == "released") {
			return w, st
		}
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", ""
	}
	if rest, ok := strings.CutPrefix(u.Path, model.WorksPath); ok {
		if w := strings.TrimSuffix(rest, "/"); validWork(w) {
			return w, ""
		}
	}
	return "", ""
}

// validWork reports whether w can be a work id: not blank, one path segment,
// no control or space characters, at most a slug's length.
func validWork(w string) bool {
	if w == "" || len(w) > model.MaxSlugLen || strings.Contains(w, "/") {
		return false
	}
	return strings.IndexFunc(w, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}
