package upcoming

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// GET /me/upcoming's answer, read for one caller from what the last run stored.

// The answer's status values.
const (
	StatusOK      = "ok"      // a run succeeded within the last 48 hours
	StatusPending = "pending" // no run has succeeded yet
	StatusStale   = "stale"   // none has for 48 hours
)

// Answer is GET /me/upcoming.
type Answer struct {
	Status      string `json:"status"`
	GeneratedAt string `json:"generated_at,omitempty"`
	WindowDays  int    `json:"window_days"`
	Items       []Item `json:"items"`
}

// Item is one work the caller's series lack.
type Item struct {
	Kind string `json:"kind"`
	// ReleaseDate is the work's first release at the precision stated
	// (YYYY, YYYY-MM or YYYY-MM-DD); omitted when unknown.
	ReleaseDate string     `json:"release_date,omitempty"`
	FirstSeenAt string     `json:"first_seen_at"`
	Work        ItemWork   `json:"work"`
	Series      ItemSeries `json:"series"`
	Local       ItemLocal  `json:"local"`
}

// ItemWork is the community work.
type ItemWork struct {
	ID       string               `json:"id"`
	Title    string               `json:"title"`
	Authors  []meta.MetaPersonRef `json:"authors"`
	CoverURL string               `json:"cover_url,omitempty"`
	WebURL   string               `json:"web_url"`
}

// ItemSeries is the community series the work is in, with its position there.
type ItemSeries struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position string `json:"position"`
}

// ItemLocal is the caller's series view the item belongs to: the library and
// the series' local spelling.
type ItemLocal struct {
	LibraryID int64  `json:"library_id"`
	Series    string `json:"series"`
}

// List is the caller's upcoming items within scopes: the items of the local
// series they hold a book of (catalog.UpcomingFor), less the works they can
// see a copy of anywhere, each work once (the first library in sort order),
// preorders soonest first, then releases newest first, then missing volumes by
// series and position. workURL names a work's community page; window is the
// recent-release window, now the clock (a preorder whose day has come since the
// last run reads as released).
func List(ctx context.Context, cat *catalog.Catalog, scopes []catalog.Scope, workURL func(string) string, window int, now time.Time) (Answer, error) {
	ans := Answer{WindowDays: window, Items: []Item{}}
	st, err := cat.UpcomingRunState(ctx)
	if err != nil {
		return ans, err
	}
	last, ok := parseStamp(st.LastSuccessAt)
	switch {
	case !ok:
		ans.Status = StatusPending
	case now.Sub(last) >= staleAfter:
		ans.Status, ans.GeneratedAt = StatusStale, st.LastSuccessAt
	default:
		ans.Status, ans.GeneratedAt = StatusOK, st.LastSuccessAt
	}
	entries, err := cat.UpcomingFor(ctx, scopes)
	if err != nil || len(entries) == 0 {
		return ans, err
	}
	ids := distinct(entries, func(e catalog.UpcomingEntry) string { return e.WorkID })
	held, err := cat.UpcomingOwnedWorks(ctx, scopes, false, ids)
	if err != nil {
		return ans, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if held[e.WorkID] || seen[e.WorkID] {
			continue
		}
		i := slices.IndexFunc(e.Series.Members, func(m catalog.MetaSeriesMember) bool { return m.WorkID == e.WorkID })
		if i < 0 {
			continue
		}
		seen[e.WorkID] = true
		m := e.Series.Members[i]
		kind := e.Kind
		if kind == catalog.UpcomingPreorder && !releaseIsFuture(e.ReleaseDate, now) {
			kind = catalog.UpcomingReleased
		}
		authors := make([]meta.MetaPersonRef, 0, len(m.Authors))
		for _, a := range m.Authors {
			authors = append(authors, meta.MetaPersonRef(a))
		}
		ans.Items = append(ans.Items, Item{
			Kind:        kind,
			ReleaseDate: e.ReleaseDate,
			FirstSeenAt: e.FirstSeenAt,
			Work:        ItemWork{ID: m.WorkID, Title: m.Title, Authors: authors, CoverURL: m.CoverURL, WebURL: workURL(m.WorkID)},
			Series:      ItemSeries{ID: e.Series.ID, Name: e.Series.Name, Position: e.Position},
			Local:       ItemLocal{LibraryID: e.LibraryID, Series: e.LocalName},
		})
	}
	slices.SortStableFunc(ans.Items, compareItems)
	return ans, nil
}

// kindRank orders the kinds: preorders, releases, missing volumes.
func kindRank(k string) int {
	switch k {
	case catalog.UpcomingPreorder:
		return 0
	case catalog.UpcomingReleased:
		return 1
	}
	return 2
}

func compareItems(a, b Item) int {
	if c := cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)); c != 0 {
		return c
	}
	switch a.Kind {
	case catalog.UpcomingPreorder:
		return cmp.Or(cmp.Compare(datePrefix(a.ReleaseDate), datePrefix(b.ReleaseDate)), cmp.Compare(a.Work.ID, b.Work.ID))
	case catalog.UpcomingReleased:
		return cmp.Or(cmp.Compare(datePrefix(b.ReleaseDate), datePrefix(a.ReleaseDate)), cmp.Compare(a.Work.ID, b.Work.ID))
	}
	pa, _ := parseSpan(a.Series.Position)
	pb, _ := parseSpan(b.Series.Position)
	return cmp.Or(
		cmp.Compare(strings.ToLower(a.Series.Name), strings.ToLower(b.Series.Name)),
		cmp.Compare(a.Series.ID, b.Series.ID),
		cmp.Compare(pa.start, pb.start),
		cmp.Compare(a.Work.ID, b.Work.ID),
	)
}
