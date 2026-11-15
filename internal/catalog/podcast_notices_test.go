package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// noticeLog collects what the SetPodcastNotice listener is told.
type noticeLog struct{ got []PodcastNotice }

func listenNotices(c *Catalog) *noticeLog {
	l := &noticeLog{}
	c.SetPodcastNotice(func(_ context.Context, n PodcastNotice) { l.got = append(l.got, n) })
	return l
}

// take returns what was told since the last take.
func (l *noticeLog) take() []PodcastNotice {
	out := l.got
	l.got = nil
	return out
}

// podcastFixture is the shared feed.json fixture for "Example Show".
func podcastFixture(t *testing.T) *feed.File {
	t.Helper()
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// withEpisode is f with one more episode, as a feed that grew lists it.
func withEpisode(f *feed.File, id, file, title, published string) *feed.File {
	g := *f
	g.Episodes = append([]feed.Episode{{ID: id, GUID: id, Path: "Example Show/" + file, Title: title, Published: published,
		EpisodeType: "full", FirstSeen: published}}, f.Episodes...)
	return &g
}

// New episodes are told only for a show indexed before as the same show: an
// unseen path published after its latest episode, once, in one notice per
// show. A first index, a forced rescan, an older episode, a re-index of
// missing rows and a replaced show at the path tell nothing.
func TestPodcastNewEpisodesNotice(t *testing.T) {
	c, ctx := newTestCatalog(t)
	// Past every date below: an episode ahead of the clock is its own test.
	c.now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	const show = "Example Show"
	f := podcastFixture(t)
	index := func(sig string, f *feed.File, force bool) {
		t.Helper()
		if _, err := c.IndexPodcastShow(ctx, lib.ID, show, sig, "", "", f, nil, force); err != nil {
			t.Fatal(err)
		}
	}
	index("s1", f, false)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a first index told %+v", got)
	}
	index("s1", f, true)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a forced rescan told %+v", got)
	}

	grown := withEpisode(f, "1111111111111111", "2026-10-26-1111111111.mp3", "Episode Four", "2026-10-26T21:00:00Z")
	grown = withEpisode(grown, "2222222222222222", "2026-10-27-2222222222.mp3", "Episode Five", "2026-10-27T21:00:00Z")
	// An episode older than the show's latest (a back catalogue the feed
	// opened up) isn't news.
	grown = withEpisode(grown, "3333333333333333", "2026-01-01-3333333333.mp3", "Old Bonus", "2026-01-01T00:00:00Z")
	index("s2", grown, false)
	got := log.take()
	if len(got) != 1 {
		t.Fatalf("told %d notices, want one for the show: %+v", len(got), got)
	}
	n := got[0]
	if n.Kind != PodcastEpisodesNew || len(n.EpisodeIDs) != 2 || !slices.Equal(n.Titles, []string{"Episode Five", "Episode Four"}) ||
		n.Show != "Example Show" || n.ShowPath != show || n.ShowID != f.ShowID || n.Library != "Pods" || n.LibraryID != lib.ID ||
		!slices.Equal(n.EpisodeIDs, []string{"2222222222222222", "1111111111111111"}) {
		t.Fatalf("notice = %+v", n)
	}
	// The same episodes again (the Bridge's rescan after the scan) are known.
	index("s3", grown, true)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a rescan told %+v again", got)
	}

	// An indexed episode the feed re-dates later is not new.
	redated := *grown
	redated.Episodes = slices.Clone(grown.Episodes)
	i := slices.IndexFunc(redated.Episodes, func(e feed.Episode) bool { return e.ID == "03fd13fa0847392f" })
	redated.Episodes[i].Published = "2026-10-30T00:00:00Z"
	index("s3b", &redated, false)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a re-dated episode told %+v", got)
	}

	// Episodes missing from the index (an older server's scan removed their
	// rows) are indexed again without news.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE library_id=?`, lib.ID); err != nil {
		t.Fatal(err)
	}
	index("s3", grown, true)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("re-indexing missing episodes told %+v", got)
	}

	// Another show at the path (a new identity) is a first index.
	other := withEpisode(grown, "4444444444444444", "2026-11-01-4444444444.mp3", "Episode Six", "2026-11-01T21:00:00Z")
	other.ShowID = "0f0f0f0f0f0f0f0f"
	index("s4", other, false)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a replaced show told %+v", got)
	}

	// More than five new episodes name the newest five.
	more := other
	for i := range 7 {
		d := string(rune('1' + i))
		more = withEpisode(more, "aaaaaaaaaaaaaaa"+d, "2026-11-1"+d+"-aaaaaaaaa"+d+".mp3", "Ep "+d, "2026-11-1"+d+"T00:00:00Z")
	}
	index("s5", more, false)
	got = log.take()
	if len(got) != 1 || len(got[0].EpisodeIDs) != 7 || len(got[0].Titles) != 5 || got[0].Titles[0] != "Ep 7" {
		t.Fatalf("seven new: %+v", got)
	}

	// No listener: nothing to tell, and the index still commits.
	c.SetPodcastNotice(nil)
	index("s6", withEpisode(more, "bbbbbbbbbbbbbbbb", "2026-12-01-bbbbbbbbbb.mp3", "Late", "2026-12-01T00:00:00Z"), false)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a cleared listener was told %+v", got)
	}
}

// One episode dated ahead of the clock (a mistyped date in the feed) is told
// once and hides nothing after it: later episodes are measured against the
// newest indexed episode that isn't ahead of the clock. With no such episode
// the future date stands, so nothing is told by mistake.
func TestPodcastNewEpisodesAfterFutureDate(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	index := func(sig string, f *feed.File) []PodcastNotice {
		t.Helper()
		if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", sig, "", "", f, nil, false); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	f := podcastFixture(t)
	index("s1", f)
	f = withEpisode(f, "1111111111111111", "2062-01-01-1111111111.mp3", "Mistyped", "2062-01-01T00:00:00Z")
	if got := index("s2", f); len(got) != 1 || !slices.Equal(got[0].Titles, []string{"Mistyped"}) {
		t.Fatalf("the future-dated episode: %+v", got)
	}
	// The next real episode is news; a back catalogue episode still isn't.
	f = withEpisode(f, "2222222222222222", "2026-11-10-2222222222.mp3", "Episode Four", "2026-11-10T21:00:00Z")
	f = withEpisode(f, "3333333333333333", "2026-01-01-3333333333.mp3", "Old Bonus", "2026-01-01T00:00:00Z")
	if got := index("s3", f); len(got) != 1 || !slices.Equal(got[0].Titles, []string{"Episode Four"}) {
		t.Fatalf("after a future-dated episode: %+v", got)
	}
	// A clock behind every indexed episode leaves the future date standing.
	now = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	f = withEpisode(f, "4444444444444444", "2026-11-12-4444444444.mp3", "Episode Five", "2026-11-12T21:00:00Z")
	if got := index("s4", f); len(got) != 0 {
		t.Fatalf("a clock behind the whole show told %+v", got)
	}
}

// pollJob is a podcast.poll for "Example Show" ending in state with code.
func pollJob(lib int64, id int64, state, code string) bridge.Job {
	show := "Example Show"
	j := bridge.Job{ID: id, Kind: bridge.KindPodcastPoll, LibraryID: &lib, Path: &show, State: state,
		Params: json.RawMessage(`{"show_id":"a1b2c3d4e5f60718"}`), Result: json.RawMessage(`{"status":200,"next_poll_seconds":900}`)}
	if state == bridge.StateFailed {
		j.Result = json.RawMessage(`{"code":"` + code + `","message":"https://user:pw@feeds.example.com/private.xml?token=SECRET failed"}`)
	}
	return j
}

// A feed is announced failing at its third failure in a row, once (its
// sixth when every failure is network-class, a mix when the shares add up);
// `gone` and a lost secret at once; the Bridge's own failures neither count
// nor reset; the next success says it is back, once; a refold tells nothing;
// a job for a replaced show is ignored.
func TestPodcastFeedNotices(t *testing.T) {
	c, ctx := newTestCatalog(t)
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", podcastFixture(t), nil, false); err != nil {
		t.Fatal(err)
	}
	next := int64(0)
	fold := func(state, code string) []PodcastNotice {
		t.Helper()
		next++
		if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, next, state, code)); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	failing := func(got []PodcastNotice, code string, failures int) {
		t.Helper()
		if len(got) != 1 || got[0].Kind != PodcastFeedFailing || got[0].Code != code || got[0].Failures != failures ||
			got[0].Show != "Example Show" || got[0].Library != "Pods" || got[0].ShowID != "a1b2c3d4e5f60718" || got[0].Private {
			t.Fatalf("want failing %s after %d, told %+v", code, failures, got)
		}
	}
	recovered := func(got []PodcastNotice) {
		t.Helper()
		if len(got) != 1 || got[0].Kind != PodcastFeedRecovered || got[0].Code != "" || got[0].Show != "Example Show" {
			t.Fatalf("want recovered, told %+v", got)
		}
	}
	quiet := func(got []PodcastNotice, what string) {
		t.Helper()
		if len(got) != 0 {
			t.Fatalf("%s told %+v", what, got)
		}
	}

	quiet(fold(bridge.StateFailed, "http_500"), "a first failure")
	quiet(fold(bridge.StateFailed, "not_a_feed"), "a second failure")
	// An offline Bridge, an unwritable library: not the feed's, so the run
	// neither grows nor resets.
	quiet(fold(bridge.StateFailed, "bridge_lost"), "a lost Bridge")
	quiet(fold(bridge.StateFailed, "library_not_writable"), "an unwritable library")
	quiet(fold(bridge.StateCancelled, ""), "a cancelled poll")
	failing(fold(bridge.StateFailed, "http_404"), "http_404", 3)
	// Refolding the same job (the recovery sweep) is a no-op.
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, next, bridge.StateFailed, "http_404")); err != nil {
		t.Fatal(err)
	}
	quiet(log.take(), "a refold")
	quiet(fold(bridge.StateFailed, "timeout"), "a failure after the announcement")
	quiet(fold(bridge.StateFailed, "gone"), "gone after the announcement")
	recovered(fold(bridge.StateDone, ""))
	quiet(fold(bridge.StateDone, ""), "a second success")
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, next, bridge.StateDone, "")); err != nil {
		t.Fatal(err)
	}
	quiet(log.take(), "a refolded success")

	// Network-class failures alone (a Bridge that lost its internet fails
	// every show this way) take six in a row.
	for i, code := range []string{"timeout", "dns", "network", "tls", "timeout"} {
		quiet(fold(bridge.StateFailed, code), fmt.Sprintf("network failure %d", i+1))
	}
	failing(fold(bridge.StateFailed, "network"), "network", 6)
	recovered(fold(bridge.StateDone, ""))
	// A mix is announced once the shares add up: two of the feed's answers
	// and two network failures.
	quiet(fold(bridge.StateFailed, "http_500"), "one answer")
	quiet(fold(bridge.StateFailed, "timeout"), "one answer, one timeout")
	quiet(fold(bridge.StateFailed, "http_503"), "two answers, one timeout")
	failing(fold(bridge.StateFailed, "dns"), "dns", 4)
	recovered(fold(bridge.StateDone, ""))

	// A success with nothing announced resets the run quietly.
	quiet(fold(bridge.StateFailed, "http_500"), "one failure")
	quiet(fold(bridge.StateFailed, "http_500"), "two failures")
	quiet(fold(bridge.StateDone, ""), "a success before the threshold")
	quiet(fold(bridge.StateFailed, "http_500"), "one failure of a new run")
	quiet(fold(bridge.StateFailed, "http_500"), "two failures of a new run")

	// gone: at once, whatever the run.
	failing(fold(bridge.StateFailed, "gone"), "gone", 3)
	recovered(fold(bridge.StateDone, ""))
	failing(fold(bridge.StateFailed, "gone"), "gone", 1)
	recovered(fold(bridge.StateDone, ""))

	// A lost private-feed secret: at once, not counted as the feed's failure.
	failing(fold(bridge.StateFailed, "feed_secret_missing"), "feed_secret_missing", 0)
	quiet(fold(bridge.StateFailed, "feed_secret_missing"), "the secret still missing")
	recovered(fold(bridge.StateDone, ""))
	// ...and says no run of the feed's failures, even after some.
	quiet(fold(bridge.StateFailed, "timeout"), "a failure before the lost secret")
	failing(fold(bridge.StateFailed, "feed_secret_missing"), "feed_secret_missing", 0)
	recovered(fold(bridge.StateDone, ""))

	// A poll for the show this path held before (another show_id) changes
	// nothing.
	quiet(fold(bridge.StateFailed, "http_500"), "one failure")
	quiet(fold(bridge.StateFailed, "timeout"), "two failures")
	stale := pollJob(lib.ID, 999, bridge.StateFailed, "http_500")
	stale.Params = json.RawMessage(`{"show_id":"0f0f0f0f0f0f0f0f"}`)
	if err := c.FoldPodcastJob(ctx, stale); err != nil {
		t.Fatal(err)
	}
	quiet(log.take(), "a replaced show's poll")
	var feedFailures, network, announced int
	if err := c.db.QueryRowContext(ctx, `SELECT feed_failures,feed_network_failures,failing_announced FROM podcast_feed_state WHERE library_id=?`, lib.ID).Scan(&feedFailures, &network, &announced); err != nil || feedFailures != 2 || network != 1 || announced != 0 {
		t.Fatalf("run = %d (%d network), %d, %v; want 2 failures, 1 network, unannounced", feedFailures, network, announced, err)
	}

	// A subscribe's failures (a follow by URL) aren't polls: they move no run.
	sub := pollJob(lib.ID, 1000, bridge.StateFailed, "http_500")
	sub.Kind, sub.Path = bridge.KindPodcastSubscribe, nil
	sub.Params = json.RawMessage(`{"show_id":"a1b2c3d4e5f60718","show_path":"Example Show","feed_url_sha256":"","feed_host":"x","feed_url":""}`)
	if err := c.FoldPodcastJob(ctx, sub); err != nil {
		t.Fatal(err)
	}
	quiet(log.take(), "a failed subscribe")
	// The stored run goes on: two answers and one timeout are not yet due
	// (were the timeout read back as an answer, they would be).
	quiet(fold(bridge.StateFailed, "http_500"), "two answers, one timeout")
	failing(fold(bridge.StateFailed, "timeout"), "timeout", 4)
}

// A private show's notices say so (notify words them without its names).
func TestPodcastNoticesMarkPrivateShows(t *testing.T) {
	c, ctx := newTestCatalog(t)
	c.now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f := podcastFixture(t)
	f.Private = true
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s1", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s2", "", "", withEpisode(f, "1111111111111111", "new.mp3", "New", "2026-12-01T00:00:00Z"), nil, false); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{bridge.StateFailed, bridge.StateDone} {
		if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, int64(i+1), state, "gone")); err != nil {
			t.Fatal(err)
		}
	}
	got := log.take()
	if len(got) != 3 || got[0].Kind != PodcastEpisodesNew || got[1].Kind != PodcastFeedFailing || got[2].Kind != PodcastFeedRecovered {
		t.Fatalf("told %+v", got)
	}
	for _, n := range got {
		if !n.Private {
			t.Fatalf("a private show's %s notice is not marked private", n.Kind)
		}
	}
}

// A show moved to another folder keeps its run of failures: a run under way
// keeps counting and a feed announced failing is told back from the new path.
func TestPodcastFeedRunMovesWithTheShow(t *testing.T) {
	c, ctx := newTestCatalog(t)
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", podcastFixture(t), nil, false); err != nil {
		t.Fatal(err)
	}
	next := int64(0)
	path := "Example Show"
	fold := func(state, code string) []PodcastNotice {
		t.Helper()
		next++
		j := pollJob(lib.ID, next, state, code)
		j.Path = &path
		if err := c.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	move := func(to string) {
		t.Helper()
		if err := c.MoveFolderFavourites(ctx, lib.ID, path, to); err != nil {
			t.Fatal(err)
		}
		moved, err := feed.ReadFile("../podcast/testdata/feed-v1.json", to, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.IndexPodcastShow(ctx, lib.ID, to, "s-"+to, "", "", moved, nil, false); err != nil {
			t.Fatal(err)
		}
		path = to
	}
	if got := fold(bridge.StateFailed, "http_500"); len(got) != 0 {
		t.Fatalf("one failure told %+v", got)
	}
	if got := fold(bridge.StateFailed, "http_500"); len(got) != 0 {
		t.Fatalf("two failures told %+v", got)
	}
	move("Moved Show")
	// The backoff and the last error start afresh at the new path.
	var failures int
	var lastError string
	if err := c.db.QueryRowContext(ctx, `SELECT failures,error_code FROM podcast_feed_state WHERE library_id=? AND show_path='Moved Show'`, lib.ID).Scan(&failures, &lastError); err != nil || failures != 0 || lastError != "" {
		t.Fatalf("moved feed state: %d %q %v", failures, lastError, err)
	}
	if got := fold(bridge.StateFailed, "http_500"); len(got) != 1 || got[0].Kind != PodcastFeedFailing || got[0].Failures != 3 || got[0].ShowPath != "Moved Show" {
		t.Fatalf("the third failure, after a move, told %+v", got)
	}
	move("Moved Again")
	if got := fold(bridge.StateDone, ""); len(got) != 1 || got[0].Kind != PodcastFeedRecovered || got[0].ShowPath != "Moved Again" {
		t.Fatalf("a success after a move told %+v", got)
	}
	var rows int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM podcast_feed_state WHERE library_id=? AND show_path<>'Moved Again'`, lib.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("feed state left at old paths: %d %v", rows, err)
	}
}
