package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// showAt is the fixture feed (show a1b2c3d4e5f60718) as the folder show holds
// it: its episode paths under show, as the Bridge writes them on taking in a
// folder renamed by hand.
func showAt(t *testing.T, show, id string) *feed.File {
	t.Helper()
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		f.ShowID = id
	}
	for i := range f.Episodes {
		f.Episodes[i].Path = show + "/" + path.Base(f.Episodes[i].Path)
	}
	return f
}

func indexShowAt(t *testing.T, c *Catalog, ctx context.Context, lib int64, show, id string) *feed.File {
	t.Helper()
	f := showAt(t, show, id)
	if _, err := c.IndexPodcastShow(ctx, lib, show, show, "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// renameFixture is a show indexed at "Example Show" with two followers, a
// policy, feed state from a poll, an episode's fetch backoff, a listener's
// place in an episode and a heart on the show folder; then the same show
// indexed at "Renamed" (the folder renamed by hand, as a rescan finds it).
type renameFixture struct {
	c          *Catalog
	ctx        context.Context
	lib        int64
	ann, bob   int64
	id, ep     string
	oldEpisode string
}

func newRenameFixture(t *testing.T) renameFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	fx := renameFixture{c: c, ctx: ctx, lib: lib.ID, ann: seedUserNamed(t, c, ctx, "ann"), bob: seedUserNamed(t, c, ctx, "bob")}
	f := indexShowAt(t, c, ctx, lib.ID, "Example Show", "")
	fx.id, fx.ep = f.ShowID, path.Base(f.Episodes[0].Path)
	fx.oldEpisode = "Example Show/" + fx.ep
	for _, u := range []int64{fx.ann, fx.bob} {
		if err := c.FollowPodcast(ctx, u, lib.ID, "Example Show", fx.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetPodcastPolicy(ctx, lib.ID, "Example Show", fx.id, &Policy{Mode: "archive", Keep: 3, Days: 7}); err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	poll := bridge.Job{ID: 1, Kind: bridge.KindPodcastPoll, LibraryID: &lib.ID, Path: &show, State: bridge.StateDone,
		Params: json.RawMessage(`{"show_id":"` + fx.id + `"}`), Result: json.RawMessage(`{"show_path":"Example Show","show_id":"` + fx.id + `","status":200,"etag":"old-etag","next_poll_seconds":3600}`)}
	if err := c.FoldPodcastJob(ctx, poll); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO podcast_fetch_state(library_id,rel_path,show_id,failures,error_code) VALUES(?,?,?,2,'timeout')`, lib.ID, fx.oldEpisode, fx.id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveProgress(ctx, fx.ann, Progress{Ref: Ref{LibraryID: lib.ID, Path: fx.oldEpisode}, Position: 42, Duration: 1720, UpdatedAt: "2026-11-01T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddFavourite(ctx, fx.bob, Ref{LibraryID: lib.ID, Path: "Example Show"}); err != nil {
		t.Fatal(err)
	}
	indexShowAt(t, c, ctx, lib.ID, "Renamed", "")
	return fx
}

func (fx renameFixture) checkMoved(t *testing.T) {
	t.Helper()
	c, ctx := fx.c, fx.ctx
	if _, err := c.PodcastShow(ctx, fx.lib, "Example Show", fx.ann); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old folder is still listed: %v", err)
	}
	for _, u := range []int64{fx.ann, fx.bob} {
		got, err := c.PodcastShow(ctx, fx.lib, "Renamed", u)
		if err != nil || !got.Following || got.Retention.Mode != "archive" || got.Status.NextCheck == nil {
			t.Fatalf("renamed show for %d: %+v %v", u, got, err)
		}
	}
	st, err := c.PodcastState(ctx, fx.lib, "Renamed")
	if err != nil || st.ETag != "old-etag" {
		t.Fatalf("feed state %+v %v", st, err)
	}
	var left int
	if err := c.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM podcast_follows WHERE show_path='Example Show')+(SELECT COUNT(*) FROM podcast_feed_state WHERE show_path='Example Show')+(SELECT COUNT(*) FROM podcast_policies WHERE show_path='Example Show')+(SELECT COUNT(*) FROM podcast_fetch_state WHERE rel_path LIKE 'Example Show/%')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left at the old folder: %d %v", left, err)
	}
	var failures int
	if err := c.db.QueryRowContext(ctx, `SELECT failures FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=?`, fx.lib, "Renamed/"+fx.ep, fx.id).Scan(&failures); err != nil || failures != 2 {
		t.Fatalf("fetch backoff %d %v", failures, err)
	}
	p, err := c.GetProgress(ctx, fx.ann, Ref{LibraryID: fx.lib, Path: "Renamed/" + fx.ep})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("episode progress %+v %v", p, err)
	}
	favs, err := c.ListAllFavourites(ctx, fx.bob, []Scope{{LibraryID: fx.lib, AllowAll: true}})
	if err != nil || len(favs) != 1 || favs[0].Path != "Renamed" {
		t.Fatalf("show heart %+v %v", favs, err)
	}
	due, err := c.DuePodcastPolls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range due {
		if r.Path == "Example Show" {
			t.Fatalf("the old folder is still polled: %+v", due)
		}
	}
}

func TestMovePodcastShowsCarriesARenamedFolder(t *testing.T) {
	fx := newRenameFixture(t)
	found, err := fx.c.PodcastIdentityPaths(fx.ctx, fx.lib, "Renamed")
	want := []PodcastIdentityPath{{"Example Show", true}, {"Renamed", true}}
	if err != nil || len(found) != 1 || len(found[fx.id]) != 2 || found[fx.id][0] != want[0] || found[fx.id][1] != want[1] {
		t.Fatalf("identity paths %+v %v", found, err)
	}
	n, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}})
	if err != nil || n != 1 {
		t.Fatalf("move %d %v", n, err)
	}
	fx.checkMoved(t)
	if found, err := fx.c.PodcastIdentityPaths(fx.ctx, fx.lib, ""); err != nil || len(found) != 0 {
		t.Fatalf("after the move %+v %v", found, err)
	}
	// Again: nothing left to move, nothing changes.
	if n, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}}); err != nil || n != 1 {
		t.Fatalf("repeat %d %v", n, err)
	}
	fx.checkMoved(t)
}

// A follow already made at the new folder (following the feed again) and the
// carried one are one follow, with the earlier date.
func TestMovePodcastShowsMergesWithTheNewFolder(t *testing.T) {
	fx := newRenameFixture(t)
	var since string
	if err := fx.c.db.QueryRowContext(fx.ctx, `SELECT created_at FROM podcast_follows WHERE user_id=?`, fx.ann).Scan(&since); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.c.db.ExecContext(fx.ctx, `INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at) VALUES(?,?,'Renamed',?,'2099-01-01T00:00:00Z')`, fx.ann, fx.lib, fx.id); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	fx.checkMoved(t)
	var n int
	var at string
	if err := fx.c.db.QueryRowContext(fx.ctx, `SELECT COUNT(*),MIN(created_at) FROM podcast_follows WHERE user_id=?`, fx.ann).Scan(&n, &at); err != nil || n != 1 || at != since {
		t.Fatalf("follows %d %s %v; want one since %s", n, at, err, since)
	}
}

// Denied: a move to a folder whose row is another show's (it changed since the
// caller looked) moves nothing.
func TestMovePodcastShowsRefusesAFolderHoldingAnotherShow(t *testing.T) {
	fx := newRenameFixture(t)
	indexShowAt(t, fx.c, fx.ctx, fx.lib, "Renamed", "0123456789abcdef")
	n, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}})
	if err != nil || n != 0 {
		t.Fatalf("move %d %v", n, err)
	}
	got, err := fx.c.PodcastShow(fx.ctx, fx.lib, "Example Show", fx.ann)
	if err != nil || !got.Following || got.Retention.Mode != "archive" {
		t.Fatalf("the old folder lost its state: %+v %v", got, err)
	}
	if p, err := fx.c.GetProgress(fx.ctx, fx.ann, Ref{LibraryID: fx.lib, Path: fx.oldEpisode}); err != nil || p == nil {
		t.Fatalf("progress moved: %v", err)
	}
}

// Two folders whose names were swapped by hand trade their shows' state in one
// call, a listener following both keeping both.
func TestMovePodcastShowsSwapsTwoFolders(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	ann := seedUser(t, c, ctx)
	x, y := "a1b2c3d4e5f60718", "0123456789abcdef"
	indexShowAt(t, c, ctx, lib.ID, "A", x)
	indexShowAt(t, c, ctx, lib.ID, "B", y)
	for _, s := range []struct{ show, id, mode string }{{"A", x, "archive"}, {"B", y, "on_demand"}} {
		if err := c.FollowPodcast(ctx, ann, lib.ID, s.show, s.id); err != nil {
			t.Fatal(err)
		}
		if err := c.SetPodcastPolicy(ctx, lib.ID, s.show, s.id, &Policy{Mode: s.mode, Keep: 3, Days: 7}); err != nil {
			t.Fatal(err)
		}
	}
	indexShowAt(t, c, ctx, lib.ID, "A", y)
	indexShowAt(t, c, ctx, lib.ID, "B", x)
	n, err := c.MovePodcastShows(ctx, lib.ID, []PodcastMove{{ShowID: x, From: []string{"A"}, To: "B"}, {ShowID: y, From: []string{"B"}, To: "A"}})
	if err != nil || n != 2 {
		t.Fatalf("swap %d %v", n, err)
	}
	for _, s := range []struct{ show, mode string }{{"A", "on_demand"}, {"B", "archive"}} {
		got, err := c.PodcastShow(ctx, lib.ID, s.show, ann)
		if err != nil || !got.Following || got.Retention.Mode != s.mode {
			t.Fatalf("%s: %+v %v", s.show, got, err)
		}
	}
}

// The poll fold: an outcome naming the folder the show was renamed to lands
// there once the old folder no longer holds the show; a copy still holding it
// keeps its own outcome.
func TestPodcastPollFoldFollowsARenamedFolder(t *testing.T) {
	fx := newRenameFixture(t)
	show := "Example Show"
	poll := bridge.Job{ID: 10, Kind: bridge.KindPodcastPoll, LibraryID: &fx.lib, Path: &show, State: bridge.StateDone,
		Params: json.RawMessage(`{"show_id":"` + fx.id + `","etag":"old-etag"}`), Result: json.RawMessage(`{"show_path":"Renamed","show_id":"` + fx.id + `","status":200,"etag":"new-etag","next_poll_seconds":3600}`)}
	// Both folders indexed (a copy, as far as the index knows): the queued one
	// keeps the outcome.
	if err := fx.c.FoldPodcastJob(fx.ctx, poll); err != nil {
		t.Fatal(err)
	}
	if st, _ := fx.c.PodcastState(fx.ctx, fx.lib, "Example Show"); st.ETag != "new-etag" {
		t.Fatalf("copy's state %+v", st)
	}
	if st, _ := fx.c.PodcastState(fx.ctx, fx.lib, "Renamed"); st.ETag != "" {
		t.Fatalf("holder's state changed by the copy's poll %+v", st)
	}
	if _, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	poll.ID = 11
	poll.Result = json.RawMessage(`{"show_path":"Renamed","show_id":"` + fx.id + `","status":200,"etag":"newer-etag","next_poll_seconds":3600}`)
	if err := fx.c.FoldPodcastJob(fx.ctx, poll); err != nil {
		t.Fatal(err)
	}
	if st, _ := fx.c.PodcastState(fx.ctx, fx.lib, "Renamed"); st.ETag != "newer-etag" {
		t.Fatalf("renamed state %+v", st)
	}
	// Denied: an outcome naming a folder that holds another show folds nowhere.
	indexShowAt(t, fx.c, fx.ctx, fx.lib, "Other", "0123456789abcdef")
	poll.ID = 12
	poll.Result = json.RawMessage(`{"show_path":"Other","show_id":"` + fx.id + `","status":200,"etag":"wrong","next_poll_seconds":3600}`)
	if err := fx.c.FoldPodcastJob(fx.ctx, poll); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Renamed", "Other"} {
		if st, _ := fx.c.PodcastState(fx.ctx, fx.lib, p); st.ETag == "wrong" {
			t.Fatalf("%s took another show's outcome", p)
		}
	}
}

// The fetch fold: an outcome naming the episode in the renamed folder lands on
// that episode once the show moved.
func TestPodcastFetchFoldFollowsARenamedFolder(t *testing.T) {
	fx := newRenameFixture(t)
	if _, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"Example Show"}, To: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	f := showAt(t, "Renamed", "")
	old := fx.oldEpisode
	fetch := bridge.Job{ID: 20, Kind: bridge.KindPodcastFetch, LibraryID: &fx.lib, Path: &old, State: bridge.StateFailed,
		Params: json.RawMessage(`{"show_id":"` + fx.id + `","episode_id":"` + f.Episodes[0].ID + `"}`),
		Result: json.RawMessage(`{"path":"Renamed/` + fx.ep + `","show_id":"` + fx.id + `","episode_id":"` + f.Episodes[0].ID + `","code":"timeout"}`)}
	if err := fx.c.FoldPodcastJob(fx.ctx, fetch); err != nil {
		t.Fatal(err)
	}
	var failures int
	var last int64
	if err := fx.c.db.QueryRowContext(fx.ctx, `SELECT failures,last_job_id FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=?`, fx.lib, "Renamed/"+fx.ep, fx.id).Scan(&failures, &last); err != nil || failures != 3 || last != 20 {
		t.Fatalf("fetch state %d %d %v", failures, last, err)
	}
}

// A listener following at two old folders (a copy removed, then the show
// renamed) keeps one follow with the earlier date, not the date of the path
// that sorts first.
func TestMovePodcastShowsKeepsTheEarliestFollow(t *testing.T) {
	fx := newRenameFixture(t)
	if _, err := fx.c.db.ExecContext(fx.ctx, `UPDATE podcast_follows SET created_at='2026-01-01T00:00:00Z' WHERE user_id=?`, fx.ann); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.c.db.ExecContext(fx.ctx, `INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at) VALUES(?,?,'A copy',?,'2026-09-01T00:00:00Z')`, fx.ann, fx.lib, fx.id); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.c.MovePodcastShows(fx.ctx, fx.lib, []PodcastMove{{ShowID: fx.id, From: []string{"A copy", "Example Show"}, To: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	fx.checkMoved(t)
	var n int
	var at string
	if err := fx.c.db.QueryRowContext(fx.ctx, `SELECT COUNT(*),MIN(created_at) FROM podcast_follows WHERE user_id=?`, fx.ann).Scan(&n, &at); err != nil || n != 1 || at != "2026-01-01T00:00:00Z" {
		t.Fatalf("follows %d %s %v", n, at, err)
	}
}

// A fetch of a show folder renamed by hand, before any scan has moved the show:
// the Bridge finds the episode in the new folder and answers done with its path
// there. The queued episode is still absent, so clearing its backoff had the
// scheduler queue it again at once, a burst of fetches the Bridge answered
// with the same reuse (57 in a second in the live run). The done fold leaves
// it cooling down, its failures as they were.
func TestPodcastFetchDoneInAnotherFolderDoesNotRequeue(t *testing.T) {
	fx := newRenameFixture(t)
	show := PodcastWorkShow{Ref: Ref{LibraryID: fx.lib, Path: "Example Show"}, ShowID: fx.id, Policy: Policy{Mode: "archive"}}
	f := showAt(t, "Example Show", "")
	var episode string
	for _, e := range f.Episodes {
		if e.Path == fx.oldEpisode {
			episode = e.ID
		}
	}
	queued := func() bool {
		t.Helper()
		got, err := fx.c.PodcastFetchCandidates(fx.ctx, show)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(got, func(c PodcastFetchCandidate) bool { return c.Path == fx.oldEpisode })
	}
	if !queued() {
		t.Fatal("the absent episode is not a fetch candidate before the fold")
	}
	old := fx.oldEpisode
	for id := int64(20); id < 23; id++ {
		done := bridge.Job{ID: id, Kind: bridge.KindPodcastFetch, LibraryID: &fx.lib, Path: &old, State: bridge.StateDone,
			Params: json.RawMessage(`{"show_id":"` + fx.id + `","episode_id":"` + episode + `"}`),
			Result: json.RawMessage(`{"path":"Renamed/` + fx.ep + `","show_id":"` + fx.id + `","episode_id":"` + episode + `","reused":true}`)}
		if err := fx.c.FoldPodcastJob(fx.ctx, done); err != nil {
			t.Fatal(err)
		}
		if queued() {
			t.Fatalf("job %d: the old folder's episode is queued again at once", id)
		}
	}
	var failures int
	var code string
	if err := fx.c.db.QueryRowContext(fx.ctx, `SELECT failures,error_code FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=?`, fx.lib, fx.oldEpisode, fx.id).Scan(&failures, &code); err != nil || failures != 2 || code != "timeout" {
		t.Fatalf("fetch state %d %q %v", failures, code, err)
	}
}
