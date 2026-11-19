package catalog

import (
	"fmt"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// CallerLibraryScope answers the library, the caller's scope and their grant
// from one read: granted is judged before private shows narrow the scope (a
// whole grant whose every show is private and unfollowed is still a grant,
// with nothing in scope), never for the old-client gate, no grant or no such
// library; an admin's unknown library is AllowAll with no library (a 404).
func TestCallerLibraryScope(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Private, f.FeedURL, f.Show.Link = true, "", ""
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false, false); err != nil {
		t.Fatal(err)
	}
	granted, none := seedUserNamed(t, c, ctx, "granted"), seedUserNamed(t, c, ctx, "none")
	if err := c.GrantWholeLibrary(ctx, granted, lib.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		who     Caller
		id      int64
		lib     bool
		granted bool
		all     bool
	}{
		{"whole grant, every show private", Caller{ID: granted, Podcasts: true}, lib.ID, true, true, false},
		{"no grant", Caller{ID: none, Podcasts: true}, lib.ID, true, false, false},
		{"old client", Caller{ID: granted}, lib.ID, true, false, false},
		{"admin, private narrowed", Caller{ID: none, Admin: true, Podcasts: true}, lib.ID, true, true, false},
		{"no such library", Caller{ID: granted, Podcasts: true}, lib.ID + 99, false, false, false},
		{"admin, no such library", Caller{ID: none, Admin: true, Podcasts: true}, lib.ID + 99, false, false, true},
	} {
		l, s, g, err := c.CallerLibraryScope(ctx, tc.who, tc.id)
		if err != nil || (l != nil) != tc.lib || g != tc.granted || s.AllowAll != tc.all || len(s.Paths) != 0 {
			t.Errorf("%s: library %v, scope %+v, granted %v, %v", tc.name, l != nil, s, g, err)
		}
		if scope, err := c.CallerScope(ctx, tc.who, tc.id); err != nil || scope.AllowAll != s.AllowAll || !slices.Equal(scope.Paths, s.Paths) {
			t.Errorf("%s: CallerScope %+v, want %+v", tc.name, scope, s)
		}
	}
}

// A show's folder listing reads one page in the show's order, with the total
// of the episodes in scope; past the end it is empty, and a scope that
// refuses the show holds none.
func TestPodcastEpisodeSlice(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f := showAt(t, "Show", "")
	tmpl := f.Episodes[0]
	f.Episodes = f.Episodes[:0]
	for i := range 5 {
		ep := tmpl
		ep.ID, ep.GUID = fmt.Sprintf("%016x", i+1), fmt.Sprintf("g%d", i)
		ep.Path, ep.Title = fmt.Sprintf("Show/ep%d.mp3", i), fmt.Sprintf("Episode %d", i)
		ep.Published = fmt.Sprintf("2020-01-0%dT00:00:00Z", i+1)
		f.Episodes = append(f.Episodes, ep)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Show", "s", "", "", f, nil, true, true); err != nil {
		t.Fatal(err)
	}
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	titles := func(eps []PodcastEpisode) (out []string) {
		for _, e := range eps {
			out = append(out, e.Title)
		}
		return out
	}
	for _, tc := range []struct {
		order         string
		offset, limit int
		want          []string
	}{
		{"newest", 0, 2, []string{"Episode 4", "Episode 3"}},
		{"newest", 3, 10, []string{"Episode 1", "Episode 0"}},
		{"oldest", 1, 2, []string{"Episode 1", "Episode 2"}},
		{"oldest", 5, 2, nil},
		{"oldest", -3, 1, []string{"Episode 0"}},
	} {
		eps, total, err := c.PodcastEpisodeSlice(ctx, lib.ID, "Show", tc.order, tc.offset, tc.limit, all)
		if err != nil || total != 5 || !slices.Equal(titles(eps), tc.want) {
			t.Errorf("%s from %d: %q of %d, %v; want %q", tc.order, tc.offset, titles(eps), total, err, tc.want)
		}
	}
	if eps, total, err := c.PodcastEpisodeSlice(ctx, lib.ID, "Show", "newest", 0, 10, Scope{LibraryID: lib.ID, Paths: []string{"Other"}}); err != nil || total != 0 || len(eps) != 0 {
		t.Errorf("out of scope: %d of %d, %v", len(eps), total, err)
	}
	if _, _, err := c.PodcastEpisodeSlice(ctx, lib.ID, "Show", "random", 0, 10, all); err == nil {
		t.Error("an unknown order was read")
	}
}

// PodcastWorkShows reads each follower's scope once a pass, and still judges
// every show against it: a listener whose share holds one of two shows they
// follow keeps only that one listed.
func TestPodcastWorkShowsFollowerScopePerShow(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	user := seedUserNamed(t, c, ctx, "listener")
	for i, show := range []string{"In Share", "Outside"} {
		f := indexShowAt(t, c, ctx, lib.ID, show, fmt.Sprintf("%016x", i+1))
		if err := c.FollowPodcast(ctx, user, lib.ID, show, f.ShowID); err != nil {
			t.Fatal(err)
		}
	}
	share, err := c.CreateShare(ctx, Share{Name: "One show", Paths: []PathRule{{LibraryID: lib.ID, Path: "In Share"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GrantShare(ctx, user, share.ID); err != nil {
		t.Fatal(err)
	}
	shows, err := c.PodcastWorkShows(ctx)
	if err != nil || len(shows) != 1 || shows[0].Path != "In Share" {
		t.Fatalf("work shows %+v %v", shows, err)
	}
}
