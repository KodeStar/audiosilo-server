package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"reflect"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func indexedFetchFixture(t *testing.T) (*Catalog, context.Context, *Library, *feed.File) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Podcasts", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]PodcastFile{path.Base(f.Episodes[0].Path): {Size: f.Episodes[0].Fetch.Bytes}}
	if _, err = c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "s", "", "", f, present, true); err != nil {
		t.Fatal(err)
	}
	return c, ctx, lib, f
}
func TestPodcastRetentionSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/retention-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Name        string  `json:"name"`
			Mode        string  `json:"mode"`
			Outside     bool    `json:"outside_keep_window"`
			Followers   int     `json:"followers"`
			AllFinished bool    `json:"all_finished"`
			FinishedAt  *string `json:"all_finished_at"`
			FetchedAt   string  `json:"fetched_at"`
			Days        int     `json:"days"`
			Starred     bool    `json:"starred"`
			Queued      bool    `json:"queued"`
			Streaming   bool    `json:"active_stream"`
			Fetching    bool    `json:"fetch_open"`
			Identity    bool    `json:"identity_matches"`
			Now         string  `json:"checked_at"`
			Eligible    bool    `json:"eligible"`
		}
	}
	if err = json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors.Cases {
		t.Run(v.Name, func(t *testing.T) {
			c, ctx, lib, f := indexedFetchFixture(t)
			ep := f.Episodes[0]
			now, err := time.Parse(time.RFC3339, v.Now)
			if err != nil {
				t.Fatal(err)
			}
			c.now = func() time.Time { return now }
			policy := Policy{Mode: v.Mode, Keep: 1, Days: v.Days}
			if err = c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID, &policy); err != nil {
				t.Fatal(err)
			}
			// The older episode is outside Keep 1. No implementation-derived expected
			// result is used: every eligibility assertion comes from the shared vector.
			if v.Outside {
				f.Episodes[0].Published = "2020-01-01T00:00:00Z"
			}
			f.Episodes[0].Fetch.FetchedAt = v.FetchedAt
			if _, err = c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "changed", "", "", f, map[string]PodcastFile{path.Base(ep.Path): {Size: ep.Fetch.Bytes}}, true); err != nil {
				t.Fatal(err)
			}
			for i := range v.Followers {
				user := seedNamedUser(t, c, string(rune('a'+i)))
				if err = c.GrantWholeLibrary(ctx, user, lib.ID); err != nil {
					t.Fatal(err)
				}
				if err = c.FollowPodcast(ctx, user, lib.ID, "Fixture Show", f.ShowID); err != nil {
					t.Fatal(err)
				}
				finished := ""
				if v.FinishedAt != nil {
					finished = *v.FinishedAt
				}
				if _, err = c.db.ExecContext(ctx, `INSERT INTO progress(user_id,library_id,rel_path,position,duration,finished,updated_at,finished_at) VALUES(?,?,?,0,4,?,?,?)`, user, lib.ID, ep.Path, v.AllFinished, "t", finished); err != nil {
					t.Fatal(err)
				}
			}
			extra := seedNamedUser(t, c, "extra")
			if v.Starred {
				if err = c.AddFavourite(ctx, extra, Ref{LibraryID: lib.ID, Path: ep.Path}); err != nil {
					t.Fatal(err)
				}
			}
			if v.Queued {
				if _, err = c.db.ExecContext(ctx, `INSERT INTO up_next VALUES(?,?,?,0,'t')`, extra, lib.ID, ep.Path); err != nil {
					t.Fatal(err)
				}
			}
			if v.Fetching {
				if _, err = c.db.ExecContext(ctx, `INSERT INTO bridge_jobs(kind,library_id,rel_path,params,dedupe_key,created_at,updated_at) VALUES('podcast.fetch',?,?,'{}','open','t','t')`, lib.ID, ep.Path); err != nil {
					t.Fatal(err)
				}
			}
			if v.Streaming {
				release := c.MarkPodcastStream(Ref{LibraryID: lib.ID, Path: ep.Path})
				defer release()
			}
			req := RetentionRequest{ShowID: f.ShowID, Path: ep.Path, Generation: ep.Fetch.Generation, SHA256: ep.Fetch.SHA256, Bytes: ep.Fetch.Bytes}
			if !v.Identity {
				req.ShowID = "0123456789abcdef"
			}
			check, err := c.PodcastRetentionCheck(ctx, lib.ID, req)
			if !v.Identity {
				if !errors.Is(err, ErrPodcastIdentity) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || check.Eligible != v.Eligible || check.Followers != v.Followers || check.AllFinished != v.AllFinished || check.Starred != v.Starred || check.Queued != v.Queued || check.ActiveStream != v.Streaming || check.FetchOpen != v.Fetching || check.OutsideKeepWindow != v.Outside {
				t.Fatalf("aggregate: %+v %v; vector %+v", check, err, v)
			}
		})
	}
}

func TestPodcastRetentionAccessAndFinishTimes(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	ep := f.Episodes[0]
	c.now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	p := Policy{Mode: "on_demand", Keep: 3, Days: 0}
	if err := c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID, &p); err != nil {
		t.Fatal(err)
	}
	a := seedNamedUser(t, c, "a")
	b := seedNamedUser(t, c, "b")
	for _, user := range []int64{a, b} {
		c.GrantWholeLibrary(ctx, user, lib.ID)
		c.FollowPodcast(ctx, user, lib.ID, "Fixture Show", f.ShowID)
	}
	req := RetentionRequest{ShowID: f.ShowID, Path: ep.Path, Generation: 2, SHA256: ep.Fetch.SHA256, Bytes: ep.Fetch.Bytes}
	check := func(followers int, eligible bool) {
		t.Helper()
		got, err := c.PodcastRetentionCheck(ctx, lib.ID, req)
		if err != nil || got.Followers != followers || got.Eligible != eligible {
			t.Fatalf("access: %+v %v", got, err)
		}
	}
	c.db.ExecContext(ctx, `INSERT INTO progress(user_id,library_id,rel_path,position,duration,finished,updated_at,finished_at) VALUES(?,?,?,1,4,1,'t','2026-10-12T08:00:00Z')`, a, lib.ID, ep.Path)
	check(2, false) // missing progress blocks
	c.db.ExecContext(ctx, `UPDATE users SET disabled=1 WHERE id=?`, b)
	check(1, true)
	c.db.ExecContext(ctx, `UPDATE users SET disabled=0,is_demo=1 WHERE id=?`, b)
	check(1, true)
	c.db.ExecContext(ctx, `UPDATE users SET is_demo=0 WHERE id=?`, b)
	c.db.ExecContext(ctx, `DELETE FROM user_share_access WHERE user_id=?`, b)
	check(1, true)
	c.db.ExecContext(ctx, `UPDATE progress SET finished_at='invalid' WHERE user_id=?`, a)
	check(1, false)
	c.db.ExecContext(ctx, `UPDATE progress SET finished_at='2026-10-12T08:00:00Z' WHERE user_id=?`, a)
	c.AddFavourite(ctx, b, Ref{LibraryID: lib.ID, Path: "Fixture Show"})
	check(1, true) // folder heart is not an episode pin
	c.AddFavourite(ctx, b, Ref{LibraryID: lib.ID, Path: ep.Path})
	check(1, false) // revoked user's exact heart still protects
}

func TestPodcastPolicyReplacementMoveAndCascade(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	p := Policy{Mode: "archive", Keep: 42, Days: 0}
	if err := c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID, &p); err != nil {
		t.Fatal(err)
	}
	old := f.ShowID
	f.ShowID = "0123456789abcdef"
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "new", "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.PodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID)
	if err != nil || got != DefaultPodcastPolicy() {
		t.Fatalf("replacement: %+v %v", got, err)
	}
	if err = c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", old, &p); !errors.Is(err, ErrPodcastIdentity) {
		t.Fatal(err)
	}
	// A newer destination preference wins a same-identity move collision.
	c.db.ExecContext(ctx, `INSERT INTO podcast_policies VALUES(?, 'Destination',?,'keep',8,1,'9999')`, lib.ID, old)
	c.db.ExecContext(ctx, `INSERT INTO podcast_fetch_state(library_id,rel_path,show_id) VALUES(?, 'Fixture Show/a.mp3',?)`, lib.ID, old)
	if err = c.MoveDurableState(ctx, lib.ID, "Fixture Show", "Destination"); err != nil {
		t.Fatal(err)
	}
	var mode string
	var keep, n int
	if err = c.db.QueryRowContext(ctx, `SELECT mode,keep FROM podcast_policies WHERE library_id=? AND show_path='Destination' AND show_id=?`, lib.ID, old).Scan(&mode, &keep); err != nil || mode != "keep" || keep != 8 {
		t.Fatalf("collision: %s %d %v", mode, keep, err)
	}
	if err = c.db.QueryRowContext(ctx, `SELECT count(*) FROM podcast_fetch_state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fetch move: %d %v", n, err)
	}
	if err = c.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.db.QueryRowContext(ctx, `SELECT count(*) FROM podcast_policies`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("purge: %d %v", n, err)
	}
}

func TestPodcastFetchFoldIsolationAndBackoff(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	ep := f.Episodes[0]
	p := ep.Path
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: f.ShowID, EpisodeID: ep.ID})
	j := bridge.Job{ID: 20, Kind: bridge.KindPodcastFetch, LibraryID: &lib.ID, Path: &p, Params: raw, State: bridge.StateFailed, Result: json.RawMessage(`{"code":"invalid_audio","message":"https://provider.example/?secret=CANARY"}`)}
	for i, want := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour} {
		j.ID = int64(20 + i)
		if err := c.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		var next, msg string
		if err := c.db.QueryRowContext(ctx, `SELECT next_attempt_at,error FROM podcast_fetch_state`).Scan(&next, &msg); err != nil || next != now.Add(want).Format(time.RFC3339) || msg != "The enclosure is not supported audio." {
			t.Fatalf("backoff %s %s %v", next, msg, err)
		}
	}
	st, err := c.PodcastState(ctx, lib.ID, "Fixture Show")
	if err != nil || st.LastChecked != nil || st.SecretMissing {
		t.Fatalf("feed fold: %+v %v", st, err)
	}
	j.ID++
	j.State = bridge.StateDone
	j.Result = json.RawMessage(`{}`)
	if err = c.FoldPodcastJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	var failures int
	var code string
	if err = c.db.QueryRowContext(ctx, `SELECT failures,error_code FROM podcast_fetch_state`).Scan(&failures, &code); err != nil || failures != 0 || code != "" {
		t.Fatalf("clear %d %s %v", failures, code, err)
	}
}

func TestPodcastRetentionWireGolden(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2a.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Request  RetentionRequest `json:"retention_check_request"`
		Response RetentionCheck   `json:"retention_check_response"`
	}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, golden.Response.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	f.Episodes[0].Published = "2020-01-01T00:00:00Z"
	f.Episodes = append(f.Episodes, feed.Episode{ID: "1111111111111111", Path: "Fixture Show/newer.mp3", Title: "Newer", Published: "2026-10-21T07:00:00Z"})
	if _, err = c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "golden", "", "", f, map[string]PodcastFile{path.Base(f.Episodes[0].Path): {Size: f.Episodes[0].Fetch.Bytes}}, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		user := seedNamedUser(t, c, name)
		if err = c.GrantWholeLibrary(ctx, user, lib.ID); err != nil {
			t.Fatal(err)
		}
		if err = c.FollowPodcast(ctx, user, lib.ID, "Fixture Show", f.ShowID); err != nil {
			t.Fatal(err)
		}
		if _, err = c.db.ExecContext(ctx, `INSERT INTO progress(user_id,library_id,rel_path,position,duration,finished,updated_at,finished_at) VALUES(?,?,?,0,4,1,'t',?)`, user, lib.ID, golden.Request.Path, *golden.Response.AllFinishedAt); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.PodcastRetentionCheck(ctx, lib.ID, golden.Request)
	if err != nil {
		t.Fatal(err)
	}
	golden.Response.LibraryID = lib.ID
	if !reflect.DeepEqual(got, golden.Response) {
		t.Fatalf("wire aggregate:\ngot %+v\nwant %+v", got, golden.Response)
	}
	// Round-trip through JSON verifies all null, boolean and integer wire fields.
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 19 {
		t.Fatalf("aggregate fields: %s", wire)
	}
}

func TestPodcastRetentionPrivateHouseholdAccess(t *testing.T) {
	c, ctx, lib, f := indexedFetchFixture(t)
	f.Private = true
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "private", "", "", f, map[string]PodcastFile{path.Base(f.Episodes[0].Path): {Size: f.Episodes[0].Fetch.Bytes}}, true); err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	policy := Policy{Mode: "on_demand", Keep: 3, Days: 0}
	if err := c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID, &policy); err != nil {
		t.Fatal(err)
	}
	owner := seedNamedUser(t, c, "owner")
	member := seedNamedUser(t, c, "member")
	if _, err := c.db.ExecContext(ctx, `UPDATE users SET role='admin' WHERE id=?1; UPDATE users SET managed_by=?1,member_kind='kid',member_access='all' WHERE id=?2`, owner, member); err != nil {
		t.Fatal(err)
	}
	if err := c.FollowPodcast(ctx, member, lib.ID, "Fixture Show", f.ShowID); err != nil {
		t.Fatal(err)
	}
	ep := f.Episodes[0]
	if _, err := c.db.ExecContext(ctx, `INSERT INTO progress(user_id,library_id,rel_path,position,duration,finished,updated_at,finished_at) VALUES(?,?,?,0,4,1,'t','2026-10-12T08:00:00Z')`, member, lib.ID, ep.Path); err != nil {
		t.Fatal(err)
	}
	req := RetentionRequest{ShowID: f.ShowID, Path: ep.Path, Generation: 2, SHA256: ep.Fetch.SHA256, Bytes: ep.Fetch.Bytes}
	got, err := c.PodcastRetentionCheck(ctx, lib.ID, req)
	if err != nil || got.Followers != 1 || !got.Eligible {
		t.Fatalf("private household: %+v %v", got, err)
	}
	if _, err = c.db.ExecContext(ctx, `UPDATE users SET role='user' WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	got, err = c.PodcastRetentionCheck(ctx, lib.ID, req)
	if err != nil || got.Followers != 0 || got.AllFinished || got.Eligible {
		t.Fatalf("revoked household: %+v %v", got, err)
	}
}
