package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	if err := c.FollowPodcast(ctx, user, lib.ID, "Example Show"); err != nil {
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
	job := bridge.Job{ID: 1, Kind: bridge.KindPodcastSubscribe, LibraryID: &lib.ID, State: bridge.StateDone, RequestedBy: &bridge.Requester{ID: user}, Params: json.RawMessage(`{}`), Result: json.RawMessage(`{"show_path":"Example Show","status":200,"etag":"abc","next_poll_seconds":1}`)}
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
