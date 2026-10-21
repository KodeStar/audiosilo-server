package catalog

import (
	"encoding/json"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func TestPodcastSecretMissingSurvivesOtherOutcomes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{bridge.KindPodcastPoll, bridge.KindPodcastSubscribe} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			lib, err := c.CreateLibrary(ctx, Library{Name: "Podcasts", Root: t.TempDir(), Kind: KindPodcasts})
			if err != nil {
				t.Fatal(err)
			}
			show := "Example Show"
			job := bridge.Job{ID: 1, Kind: kind, LibraryID: &lib.ID, Path: &show,
				Params: json.RawMessage(`{"show_path":"Example Show"}`),
				State:  bridge.StateFailed, Result: json.RawMessage(`{"code":"feed_secret_missing"}`)}
			fold := func() {
				t.Helper()
				if err := c.FoldPodcastJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				job.ID++
			}
			check := func(missing bool, code string) {
				t.Helper()
				st, err := c.PodcastState(ctx, lib.ID, show)
				if err != nil || st.SecretMissing != missing || st.ErrorCode != code {
					t.Fatalf("state: %+v %v, missing=%v code=%q", st, err, missing, code)
				}
			}
			fold()
			check(true, "feed_secret_missing")
			if err := c.PodcastNotWritable(ctx, Ref{LibraryID: lib.ID, Path: show}); err != nil {
				t.Fatal(err)
			}
			check(true, "library_not_writable")
			job.Result = json.RawMessage(`{"code":"timeout"}`)
			fold()
			check(true, "timeout")
			job.State = bridge.StateCancelled
			fold()
			check(true, "timeout")
			job.State = bridge.StateDone
			job.Kind = kind
			job.Params = json.RawMessage(`{}`)
			job.Result = json.RawMessage(`{"show_path":"Example Show","status":304}`)
			fold()
			check(false, "")
			job.State = bridge.StateFailed
			job.Kind = bridge.KindPodcastPoll
			job.Result = json.RawMessage(`{"code":"timeout"}`)
			fold()
			check(false, "timeout")
		})
	}
}

func TestPodcastSubscribeFollowsReportedIdentity(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	user := seedUser(t, c, ctx)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Podcasts", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Private = true
	oldID := f.ShowID
	f.ShowID = "0123456789abcdef"
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "replacement", "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	job := bridge.Job{ID: 1, Kind: bridge.KindPodcastSubscribe, LibraryID: &lib.ID, State: bridge.StateDone,
		RequestedBy: &bridge.Requester{ID: user}, Params: json.RawMessage(`{}`)}
	for _, id := range []string{oldID, f.ShowID} {
		job.Result, err = json.Marshal(map[string]any{"show_path": "Example Show", "show_id": id, "status": 200})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.FoldPodcastJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := c.PodcastShow(ctx, lib.ID, "Example Show", user)
		if err != nil || got.Following != (id == f.ShowID) {
			t.Fatalf("fold identity %s: %+v %v", id, got, err)
		}
		// The followed-only query must exclude stale follows even with an
		// unfiltered content scope, including when a replacement is public.
		list, err := c.PodcastShows(ctx, Scope{LibraryID: lib.ID, AllowAll: true}, user, true)
		if err != nil || (len(list) == 1) != got.Following {
			t.Fatalf("followed shows: %+v %v", list, err)
		}
		job.ID++
	}
	// An outcome without a valid identity records no follow that could never match.
	job.Result = json.RawMessage(`{"show_path":"Example Show","status":200}`)
	if err := c.UnfollowPodcast(ctx, user, lib.ID, "Example Show"); err != nil {
		t.Fatal(err)
	}
	if err := c.FoldPodcastJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM podcast_follows WHERE user_id=?`, user).Scan(&n); err != nil || n != 0 {
		t.Fatalf("follow without identity: %d %v", n, err)
	}
}
