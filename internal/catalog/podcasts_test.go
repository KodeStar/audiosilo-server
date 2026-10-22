package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func TestPodcastRootsAndKind(t *testing.T) {
	c, ctx := newTestCatalog(t)
	root := t.TempDir()
	books, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: filepath.Join(root, "books")})
	if err != nil {
		t.Fatal(err)
	}
	if books.Kind != KindBooks {
		t.Fatal(books.Kind)
	}
	for i, p := range []string{books.Root, filepath.Join(books.Root, "inside"), root} {
		if _, err := c.CreateLibrary(ctx, Library{Name: string(rune('A' + i)), Root: p, Kind: KindPodcasts}); !errors.Is(err, ErrRootOverlaps) {
			t.Fatalf("overlap %s %v", p, err)
		}
	}
	podcasts, err := c.CreateLibrary(ctx, Library{Name: "Podcasts", Root: filepath.Join(root, "podcasts"), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.UpdateLibrary(ctx, books.ID, LibraryPatch{Root: filepath.Join(podcasts.Root, "nested")}); !errors.Is(err, ErrRootOverlaps) {
		t.Fatalf("reroot %v", err)
	}
	changed := KindPodcasts
	if _, _, err := c.UpdateLibrary(ctx, books.ID, LibraryPatch{Kind: &changed}); !errors.Is(err, ErrKindImmutable) {
		t.Fatalf("rekind %v", err)
	}
	if _, err := c.CreateLibrary(ctx, Library{Name: "Bad", Root: "/bad", Kind: "music"}); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("kind %v", err)
	}
}
func TestPodcastScopesAndAggregates(t *testing.T) {
	c, ctx := newTestCatalog(t)
	user := seedUser(t, c, ctx)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Private = true
	f.FeedURL = ""
	f.Show.Link = ""
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	who := Caller{ID: user, Admin: true, Podcasts: true}
	s, err := c.CallerScope(ctx, who, lib.ID)
	if err != nil || s.AllowAll || len(s.Paths) != 0 {
		t.Fatalf("private admin scope %+v %v", s, err)
	}
	if err := c.FollowPodcast(ctx, user, lib.ID, "Example Show", f.ShowID); err != nil {
		t.Fatal(err)
	}
	s, err = c.CallerScope(ctx, who, lib.ID)
	if err != nil || !s.AllowAll {
		t.Fatalf("follower %+v %v", s, err)
	}
	who.Podcasts = false
	s, err = c.CallerScope(ctx, who, lib.ID)
	if err != nil || s.AllowAll || len(s.Paths) != 0 {
		t.Fatalf("old admin %+v %v", s, err)
	}
	scope := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	recent, err := c.RecentBooks(ctx, scope, 50)
	if err != nil || len(recent) != 0 {
		t.Fatalf("recent %+v %v", recent, err)
	}
	people, err := c.People(ctx, PeopleAuthors, lib.ID, nil)
	if err != nil || len(people.People) != 0 {
		t.Fatalf("people %+v %v", people, err)
	}
	admin, err := c.ListAdminBooks(ctx, AdminListOptions{})
	if err != nil || len(admin.Books) != 0 {
		t.Fatalf("admin %+v %v", admin, err)
	}
	if _, err := c.ExportLibraryBooks(ctx, lib.ID, "test", ExportOptions{}); !errors.Is(err, ErrNotBookLibrary) {
		t.Fatalf("export %v", err)
	}
	checks, err := c.DueChapterChecks(ctx, "9999", 100)
	if err != nil || len(checks) != 0 {
		t.Fatalf("chapters %+v %v", checks, err)
	}
	colors, _, err := c.CoverColorsDue(ctx, 0, 100)
	if err != nil || len(colors) != 0 {
		t.Fatalf("colors %+v %v", colors, err)
	}
}

// A new metadata source re-resolves a books library; a podcast library's
// episodes come from feed.json and must keep what it said.
func TestPodcastMetadataSourceKeepsEpisodes(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{MetadataFromPath, MetadataFromTags} {
		if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &source}); err != nil {
			t.Fatal(err)
		}
		b, err := c.GetBookByPath(ctx, lib.ID, f.Episodes[0].Path)
		if err != nil || b.Title != f.Episodes[0].Title || b.Description != f.Episodes[0].Description || b.Author != f.Show.Author {
			t.Fatalf("%s: %+v %v", source, b, err)
		}
	}
}

// A prune removes only the shows it names (a scan's, known at its start), so a
// show indexed while the scan ran stays.
func TestPodcastPruneNamedShows(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.PrunePodcastShows(ctx, lib.ID, []string{"Gone Show"}); err != nil {
		t.Fatal(err)
	}
	if b, err := c.GetBookByPath(ctx, lib.ID, f.Episodes[0].Path); err != nil || b.Episode == nil {
		t.Fatalf("unnamed show pruned: %+v %v", b, err)
	}
	if err := c.PrunePodcastShows(ctx, lib.ID, []string{"Example Show"}); err != nil {
		t.Fatal(err)
	}
	sigs, err := c.PodcastSignatures(ctx, lib.ID)
	if _, gone := c.GetBookByPath(ctx, lib.ID, f.Episodes[0].Path); err != nil || len(sigs) != 0 || !errors.Is(gone, ErrNotFound) {
		t.Fatalf("named show kept: %v %v %v", sigs, err, gone)
	}
}

// Episodes are no books in the console's activity figures or a listening goal.
func TestPodcastEpisodesOutOfBookFigures(t *testing.T) {
	c, ctx := newTestCatalog(t)
	user := seedUser(t, c, ctx)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveProgress(ctx, user, Progress{Ref: Ref{LibraryID: lib.ID, Path: f.Episodes[0].Path},
		Position: 600, Duration: 600, Finished: true, UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	a, err := c.ActivityFor(ctx, "7d", now.AddDate(0, 0, -7), now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if a.Coverage.Books != 0 || a.Growth[len(a.Growth)-1].Books != 0 || a.Totals != (ActivityTotals{}) || a.Funnel != (Funnel{}) {
		t.Fatalf("activity coverage %+v growth %+v totals %+v funnel %+v", a.Coverage, a.Growth, a.Totals, a.Funnel)
	}
	goal, err := c.GoalStatusFor(ctx, user, now, time.UTC)
	if err != nil || goal.Finished != 0 {
		t.Fatalf("goal %+v %v", goal, err)
	}
}

func TestPodcastFolding(t *testing.T) {
	c, ctx := newTestCatalog(t)
	user := seedUser(t, c, ctx)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	job := bridge.Job{ID: 1, Kind: bridge.KindPodcastSubscribe, LibraryID: &lib.ID, State: bridge.StateDone, RequestedBy: &bridge.Requester{ID: user}, Params: json.RawMessage(`{}`), Result: json.RawMessage(`{"show_path":"Example Show","show_id":"a1b2c3d4e5f60718","status":200,"etag":"abc","next_poll_seconds":1}`)}
	if err := c.FoldPodcastJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := c.PodcastShow(ctx, lib.ID, "Example Show", user)
	if err != nil || !got.Following || got.Status.NextCheck == nil || *got.Status.NextCheck != "2026-10-20T12:15:00Z" {
		t.Fatalf("fold %+v %v", got, err)
	}
	_ = c.UnfollowPodcast(ctx, user, lib.ID, "Example Show")
	if err := c.FoldPodcastJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, _ = c.PodcastShow(ctx, lib.ID, "Example Show", user)
	if got.Following {
		t.Fatal("fold sweep undid unfollow")
	}
	job.Kind = bridge.KindPodcastPoll
	job.Params = json.RawMessage(`{"show_id":"a1b2c3d4e5f60718"}`)
	show := "Example Show"
	job.Path = &show
	for i, code := range []string{"timeout", "timeout", "gone"} {
		job.ID = int64(i + 2)
		job.State = bridge.StateFailed
		job.Result = json.RawMessage(`{"code":"` + code + `","message":"failure"}`)
		if err := c.FoldPodcastJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err = c.PodcastShow(ctx, lib.ID, show, user)
		if err != nil {
			t.Fatal(err)
		}
		if code == "gone" {
			if got.Status.NextCheck != nil {
				t.Fatal("gone still scheduled")
			}
		} else {
			t1, _ := time.Parse(time.RFC3339, *got.Status.NextCheck)
			want := time.Duration(1800<<i) * time.Second
			if t1.Sub(now) != want {
				t.Fatalf("backoff %v want %v", t1.Sub(now), want)
			}
		}
	}
	job.ID = 5
	job.State = bridge.StateDone
	job.Result = json.RawMessage(`{"status":304,"etag":"","next_poll_seconds":999999}`)
	if err := c.FoldPodcastJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	st, err := c.PodcastState(ctx, lib.ID, show)
	if err != nil || st.ETag != "abc" || st.ErrorCode != "" {
		t.Fatalf("304 %+v %v", st, err)
	}
	for i, tc := range []struct{ retry, want int }{{0, 60}, {-1, 60}, {999999, 86400}} {
		job.ID = int64(6 + i)
		job.State = bridge.StateFailed
		job.Result = json.RawMessage(fmt.Sprintf(`{"code":"timeout","retry_after_seconds":%d}`, tc.retry))
		if err := c.FoldPodcastJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := c.PodcastShow(ctx, lib.ID, show, user)
		if err != nil || got.Status.NextCheck == nil {
			t.Fatalf("retry %+v %v", got, err)
		}
		at, err := time.Parse(time.RFC3339, *got.Status.NextCheck)
		if err != nil || at.Sub(now) != time.Duration(tc.want)*time.Second {
			t.Fatalf("retry %d: %s %v", tc.retry, *got.Status.NextCheck, err)
		}
	}

}

// A validator longer than a poll may carry is dropped, and a field of the wrong
// type doesn't fail the fold (the sweep would retry it on every tick).
func TestPodcastFoldingOddOutcomes(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	long := strings.Repeat("�", 200) // 600 bytes: obs-text the Bridge encoded
	job := bridge.Job{ID: 1, Kind: bridge.KindPodcastPoll, Params: json.RawMessage(`{"show_id":"a1b2c3d4e5f60718"}`), LibraryID: &lib.ID, Path: &show, State: bridge.StateDone,
		Result: json.RawMessage(`{"status":200,"etag":"` + long + `","last_modified":"Mon, 20 Oct 2026 07:59:00 GMT","next_poll_seconds":"soon"}`)}
	if err := c.FoldPodcastJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	st, err := c.PodcastState(ctx, lib.ID, show)
	if err != nil || st.ETag != "" || st.LastModified != "Mon, 20 Oct 2026 07:59:00 GMT" || st.LastChecked == nil {
		t.Fatalf("state %+v %v", st, err)
	}
	var p bridge.PollParams
	if err := json.Unmarshal(st.PollParams(), &p); err != nil || len(p.ETag) > bridge.MaxPollValidator {
		t.Fatalf("params %s %v", st.PollParams(), err)
	}
}
