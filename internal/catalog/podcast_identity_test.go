package catalog

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func TestPodcastFeedStateReplacementIsolation(t *testing.T) {
	for _, code := range []string{"gone", "timeout", "feed_secret_missing"} {
		t.Run(code, func(t *testing.T) {
			c, ctx, lib, f := indexedFetchFixture(t)
			show := "Fixture Show"
			old := f.ShowID
			params, _ := json.Marshal(bridge.PollParams{ShowID: old})
			result, _ := json.Marshal(bridge.PodcastOutcome{Code: code, Message: "safe failure", ETag: "old-etag"})
			job := bridge.Job{ID: 1, Kind: bridge.KindPodcastPoll, LibraryID: &lib.ID, Path: &show, State: bridge.StateFailed, Params: params, Result: result}
			if err := c.FoldPodcastJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			f.ShowID = "0123456789abcdef"
			if _, err := c.IndexPodcastShow(ctx, lib.ID, show, "replacement", "", "", f, nil, true); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				st, err := c.PodcastState(ctx, lib.ID, show)
				if err != nil || st.ShowID != f.ShowID || st.ETag != "" || st.LastChecked != nil || st.ErrorCode != "" || st.SecretMissing {
					t.Fatalf("replacement state: %+v %v", st, err)
				}
			}
			check()
			for _, state := range []string{bridge.StateFailed, bridge.StateDone} {
				job.ID++
				job.State = state
				job.Result = json.RawMessage(`{"show_id":"a1b2c3d4e5f60718","etag":"stale","status":200,"code":"gone"}`)
				if err := c.FoldPodcastJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				check()
			}
			// Pre-upgrade polls have no expected identity and cannot recreate status.
			job.ID++
			job.Params = json.RawMessage(`{}`)
			job.Result = json.RawMessage(`{"etag":"unbound","status":200}`)
			if err := c.FoldPodcastJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			check()
			// A repairing subscribe bound to the old show cannot adopt a new result ID.
			job.ID++
			job.Kind = bridge.KindPodcastSubscribe
			job.Params = json.RawMessage(`{"show_path":"Fixture Show","show_id":"a1b2c3d4e5f60718"}`)
			job.Result = json.RawMessage(`{"show_path":"Fixture Show","show_id":"0123456789abcdef","etag":"repair-stale","status":200}`)
			if err := c.FoldPodcastJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			check()
			user := seedUser(t, c, ctx)
			if err := c.FollowPodcast(ctx, user, lib.ID, show, f.ShowID); err != nil {
				t.Fatal(err)
			}
			refs, err := c.DuePodcastPolls(ctx, 10)
			if err != nil || len(refs) != 1 {
				t.Fatalf("fresh due %+v %v", refs, err)
			}
		})
	}
}

func TestPodcastGenerationIndexPreservesListeningState(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	ep := &f.Episodes[0]
	user := seedUser(t, c, ctx)
	ref := Ref{LibraryID: lib.ID, Path: ep.Path}
	initial, err := c.SaveProgress(ctx, user, Progress{Ref: ref, Position: 2, Duration: 4.056, Finished: true})
	if err != nil {
		t.Fatal(err)
	}
	bm, err := c.AddBookmark(ctx, user, Bookmark{Ref: ref, Position: 1, Note: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	fetch := *ep.Fetch
	// Generation facts are content metadata. Tombstones, new audio and a rebuild
	// must not rewrite path-keyed seconds, finished state or bookmarks.
	for _, state := range []string{"tombstone", "new_generation", "missing", "rebuild"} {
		fetch.Generation = 2
		fetch.TimelineChanged = true
		fetch.DeletedAt = ""
		present := map[string]PodcastFile{}
		if state == "tombstone" {
			fetch.DeletedAt = "2026-10-22T08:00:00Z"
		}
		if state == "new_generation" {
			fetch.Generation = 3
			fetch.SHA256 = "b9e2206c3fd1f9a9e9c78e9b467d888926e0396ec49760736f78705920ba8dda"
			present["2026-10-19-1cc306d831.mp3"] = PodcastFile{Size: fetch.Bytes}
		}
		ep.Fetch = &fetch
		if _, err = c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", state, "", "", f, present, true); err != nil {
			t.Fatal(err)
		}
		got, err := c.GetProgress(ctx, user, ref)
		if err != nil || !reflect.DeepEqual(got, initial) {
			t.Fatalf("progress %s: %+v vs %+v %v", state, got, initial, err)
		}
		marks, err := c.ListBookmarks(ctx, user, ref)
		if err != nil || len(marks) != 1 || marks[0].ID != bm.ID || marks[0].Position != 1 {
			t.Fatalf("bookmarks %s %+v %v", state, marks, err)
		}
		b, err := c.GetBookByPath(ctx, lib.ID, ep.Path)
		if err != nil || b.Episode.TimelineChanged == nil || !*b.Episode.TimelineChanged || b.Episode.Generation != fetch.Generation {
			t.Fatalf("generation %s %+v %v", state, b, err)
		}
		if !feed.ValidFetch(ep.Fetch) {
			t.Fatal("invalid test pin")
		}
	}
}
