package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// noticeLog collects the notices the SetPodcastNotice listener delivers (each
// recorded as an event whose data index the notices built).
type noticeLog struct{ built, got []PodcastNotice }

func listenNotices(c *Catalog) *noticeLog {
	l := &noticeLog{}
	c.SetPodcastNotice(&PodcastListener{
		Event: func(n PodcastNotice) (string, map[string]any, string) {
			l.built = append(l.built, n)
			return "podcast_" + n.Kind, map[string]any{"n": len(l.built) - 1}, ""
		},
		Deliver: func(_ context.Context, ev ServerEvent) {
			l.got = append(l.got, l.built[int(ev.Data["n"].(float64))])
		},
	})
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
	// Just past the episodes each step adds (the Bridge's rescan follows its
	// poll): an episode ahead of the clock is its own test.
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
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
	grown.Episodes[0].FirstSeen = "2026-10-27T22:00:00Z" // first seen with the others
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
	now = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
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
	f = withEpisode(f, "2222222222222222", "2026-11-14-2222222222.mp3", "Episode Four", "2026-11-14T21:00:00Z")
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
		Params: json.RawMessage(`{"show_id":"a1b2c3d4e5f60718","scheduled":true}`), Result: json.RawMessage(`{"status":200,"next_poll_seconds":900}`)}
	if state == bridge.StateFailed {
		j.Result = json.RawMessage(`{"code":"` + code + `","message":"https://user:pw@feeds.example.com/private.xml?token=SECRET failed"}`)
	}
	return j
}

// followerPoll is pollJob as a follower's Check now (unscheduled), asked by
// by (nil: a requester since deleted).
func followerPoll(lib, id int64, state, code string, by *bridge.Requester) bridge.Job {
	j := pollJob(lib, id, state, code)
	j.RequestedBy, j.Params = by, json.RawMessage(`{"show_id":"a1b2c3d4e5f60718"}`)
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
	quiet(fold(bridge.StateFailed, "http_500"), "another code after the announcement")
	// gone and a lost secret stop the feed for good: each is told once more
	// after the announcement said another code (server#403), once.
	failing(fold(bridge.StateFailed, "gone"), "gone", 6)
	quiet(fold(bridge.StateFailed, "gone"), "gone again")
	failing(fold(bridge.StateFailed, "feed_secret_missing"), "feed_secret_missing", 0)
	quiet(fold(bridge.StateFailed, "feed_secret_missing"), "the secret still missing")
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
	recovered(fold(bridge.StateDone, ""))

	// A follower's Check now (a poll with a requester) is not counted toward
	// the run (server#403): five in a row announce nothing and leave it as
	// it was, so the scheduled polls still take three. Its gone is still
	// announced at once (polling stops on it), and its success still says
	// the feed is back.
	checkNow := func(state, code string) []PodcastNotice {
		t.Helper()
		next++
		j := followerPoll(lib.ID, next, state, code, &bridge.Requester{ID: 7, Username: "follower"})
		if err := c.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	quiet(fold(bridge.StateFailed, "http_500"), "a scheduled failure")
	// Nor does it back the scheduled polls off: they would come a day apart,
	// and the notice with them. Its Retry-After still holds them off, never
	// brings them forward.
	backoff := func() (failures int, next string) {
		t.Helper()
		if err := c.db.QueryRowContext(ctx, `SELECT failures,next_poll_at FROM podcast_feed_state WHERE library_id=? AND show_path=?`, lib.ID, "Example Show").Scan(&failures, &next); err != nil {
			t.Fatal(err)
		}
		return failures, next
	}
	failures, due := backoff()
	for i := range 5 {
		quiet(checkNow(bridge.StateFailed, "http_500"), fmt.Sprintf("Check now failure %d", i+1))
	}
	if f, at := backoff(); f != failures || at != due {
		t.Fatalf("Check now failures moved the scheduled polls: %d failures, next %s, want %d, %s", f, at, failures, due)
	}
	retryAfter := func(seconds int) {
		t.Helper()
		next++
		j := followerPoll(lib.ID, next, bridge.StateFailed, "http_429", &bridge.Requester{ID: 7, Username: "follower"})
		j.Result = json.RawMessage(fmt.Sprintf(`{"code":"http_429","retry_after_seconds":%d}`, seconds))
		if err := c.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		quiet(log.take(), "a Check now told to retry later")
	}
	retryAfter(60)
	if f, at := backoff(); f != failures || at != due {
		t.Fatalf("a short Retry-After brought the scheduled poll forward: %d failures, next %s, want %d, %s", f, at, failures, due)
	}
	retryAfter(7200)
	if f, at := backoff(); f != failures || at <= due {
		t.Fatalf("a Retry-After past the scheduled poll: %d failures, next %s, want %d and later than %s", f, at, failures, due)
	}
	quiet(fold(bridge.StateFailed, "http_503"), "a second scheduled failure")
	failing(fold(bridge.StateFailed, "http_404"), "http_404", 3)
	recovered(checkNow(bridge.StateDone, ""))
	failing(checkNow(bridge.StateFailed, "gone"), "gone", 1)
}

// A run is due once its shares add up to one whole: a third of the way per
// feed answer, a sixth per network failure.
func TestFeedRunDue(t *testing.T) {
	for _, c := range []struct {
		answers, network int
		due              bool
	}{
		{2, 0, false}, {3, 0, true},
		{0, 5, false}, {0, 6, true},
		{1, 3, false}, {1, 4, true},
		{2, 1, false}, {2, 2, true},
	} {
		r := feedRun{failures: c.answers + c.network, network: c.network}
		if r.due() != c.due {
			t.Errorf("%d answers, %d network: due = %v, want %v", c.answers, c.network, r.due(), c.due)
		}
	}
}

// A private show's notices say so (notify words them without its names).
func TestPodcastNoticesMarkPrivateShows(t *testing.T) {
	c, ctx := newTestCatalog(t)
	c.now = func() time.Time { return time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC) }
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

// podcastShowsLib is a podcasts library holding the fixture indexed as "Example
// Show" and, with others, more shows of their own identity (folders "Show 1"..).
func podcastShowsLib(t *testing.T, c *Catalog, ctx context.Context, others int) *Library {
	t.Helper()
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", podcastFixture(t), nil, false); err != nil {
		t.Fatal(err)
	}
	for i := range others {
		f := podcastFixture(t)
		f.ShowID = fmt.Sprintf("b%015d", i+1)
		if _, err := c.IndexPodcastShow(ctx, lib.ID, fmt.Sprintf("Show %d", i+1), "s", "", "", f, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	return lib
}

// showPoll is a scheduled podcast.poll of the n-th other show (0: the
// fixture's "Example Show") ending in state with code.
func showPoll(lib, id int64, n int, state, code string) bridge.Job {
	j := pollJob(lib, id, state, code)
	if n > 0 {
		show := fmt.Sprintf("Show %d", n)
		j.Path, j.Params = &show, json.RawMessage(fmt.Sprintf(`{"show_id":"b%015d","scheduled":true}`, n))
	}
	return j
}

// A network-class failure counts only while the Bridge reaches other feeds
// (server#403): a Bridge that lost its internet fails every show and
// announces none of them however long it lasts, while a feed failing alone
// among shows the Bridge still reaches is announced at its sixth. A lone show
// (no other feed to judge the Bridge by) counts as before.
func TestPodcastNetworkFailuresJudgedAcrossTheBridge(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 2)
	id := int64(0)
	fold := func(n int, state, code string) {
		t.Helper()
		id++
		now = now.Add(10 * time.Minute)
		if err := c.FoldPodcastJob(ctx, showPoll(lib.ID, id, n, state, code)); err != nil {
			t.Fatal(err)
		}
	}
	// Every show checks fine, then the Bridge's internet goes: twenty rounds
	// of every show failing announce nothing.
	for n := range 3 {
		fold(n, bridge.StateDone, "")
	}
	for range 20 {
		for n, code := range []string{"timeout", "dns", "network"} {
			fold(n, bridge.StateFailed, code)
		}
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a Bridge outage told %+v", got)
	}
	var run int
	if err := c.db.QueryRowContext(ctx, `SELECT feed_failures FROM podcast_feed_state WHERE library_id=? AND show_path='Example Show'`, lib.ID).Scan(&run); err != nil || run > 1 {
		t.Fatalf("an outage counted %d failures (%v), want at most the first", run, err)
	}
	// The internet is back: the other shows check fine, this one doesn't.
	for range 6 {
		fold(1, bridge.StateDone, "")
		fold(2, bridge.StateDone, "")
		fold(0, bridge.StateFailed, "timeout")
	}
	got := log.take()
	if len(got) != 1 || got[0].Kind != PodcastFeedFailing || got[0].ShowPath != "Example Show" || got[0].Code != "timeout" || got[0].Failures != NetworkFailingAfter {
		t.Fatalf("a feed failing among reachable ones told %+v, want failing timeout after %d", got, NetworkFailingAfter)
	}

	// A lone show: nothing to judge the Bridge by, so each failure counts.
	c2, ctx2 := newTestCatalog(t)
	log2 := listenNotices(c2)
	lone := podcastShowsLib(t, c2, ctx2, 0)
	for i := range NetworkFailingAfter {
		if err := c2.FoldPodcastJob(ctx2, showPoll(lone.ID, int64(i+1), 0, bridge.StateFailed, "timeout")); err != nil {
			t.Fatal(err)
		}
	}
	if got := log2.take(); len(got) != 1 || got[0].Failures != NetworkFailingAfter {
		t.Fatalf("a lone show told %+v", got)
	}

	// Another feed failing with an HTTP error was reached all the same: a
	// dead host's timeouts beside it count; a check failed at the Bridge
	// (bridge_lost) says nothing of its internet either way.
	c3, ctx3 := newTestCatalog(t)
	now3 := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c3.now = func() time.Time { return now3 }
	log3 := listenNotices(c3)
	pair := podcastShowsLib(t, c3, ctx3, 2)
	id3 := int64(0)
	fold3 := func(n int, state, code string) {
		t.Helper()
		id3++
		now3 = now3.Add(10 * time.Minute)
		if err := c3.FoldPodcastJob(ctx3, showPoll(pair.ID, id3, n, state, code)); err != nil {
			t.Fatal(err)
		}
	}
	for n := range 3 {
		fold3(n, bridge.StateDone, "")
	}
	for range NetworkFailingAfter {
		fold3(0, bridge.StateFailed, "timeout")
		fold3(1, bridge.StateFailed, "http_500")
		fold3(2, bridge.StateFailed, "bridge_lost")
	}
	var dead []PodcastNotice
	for _, n := range log3.take() {
		if n.ShowPath == "Example Show" {
			dead = append(dead, n)
		}
	}
	if len(dead) != 1 || dead[0].Code != "timeout" || dead[0].Failures != NetworkFailingAfter {
		t.Fatalf("a dead host beside an answering feed told %+v, want failing timeout after %d", dead, NetworkFailingAfter)
	}
}

// A round's polls can be folded within one second (the stamps are whole
// seconds): the round's first show still sees the others' checks of the round
// before, and an outage counts at most its first failure. A feed failing alone
// in such a round, the others answering in its second, counts.
func TestPodcastOutageFoldedInOnePass(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 2)
	id := int64(0)
	round := func(state string, codes ...string) {
		t.Helper()
		now = now.Add(30 * time.Minute)
		for n, code := range codes {
			id++
			if err := c.FoldPodcastJob(ctx, showPoll(lib.ID, id, n, state, code)); err != nil {
				t.Fatal(err)
			}
		}
	}
	failures := func(show string) int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT feed_failures FROM podcast_feed_state WHERE library_id=? AND show_path=?`, lib.ID, show).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	round(bridge.StateDone, "", "", "")
	for range 20 {
		round(bridge.StateFailed, "timeout", "dns", "network")
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("an outage folded a round at a time told %+v", got)
	}
	for _, show := range []string{"Example Show", "Show 1", "Show 2"} {
		if n := failures(show); n > 1 {
			t.Fatalf("an outage counted %d failures of %s, want at most the first", n, show)
		}
	}
	round(bridge.StateDone, "", "", "")
	for range NetworkFailingAfter {
		now = now.Add(30 * time.Minute)
		for n, state := range []string{bridge.StateFailed, bridge.StateDone, bridge.StateDone} {
			id++
			code := ""
			if state == bridge.StateFailed {
				code = "timeout"
			}
			if err := c.FoldPodcastJob(ctx, showPoll(lib.ID, id, n, state, code)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := log.take(); len(got) != 1 || got[0].ShowPath != "Example Show" || got[0].Failures != NetworkFailingAfter {
		t.Fatalf("a feed failing alone in its round told %+v, want failing after %d", got, NetworkFailingAfter)
	}
}

// Only the scheduler's polls count (bridge.PollParams.Scheduled): a Check now
// stays uncounted even once its requester is gone (requested_by is then NULL,
// which used to read as the scheduler's).
func TestPodcastCheckNowWithoutRequesterIsNotCounted(t *testing.T) {
	c, ctx := newTestCatalog(t)
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 0)
	for i := range 5 {
		if err := c.FoldPodcastJob(ctx, followerPoll(lib.ID, int64(i+1), bridge.StateFailed, "http_500", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("Check now failures told %+v", got)
	}
	for i := range FeedFailingAfter {
		if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, int64(10+i), bridge.StateFailed, "http_500")); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.take(); len(got) != 1 || got[0].Failures != FeedFailingAfter {
		t.Fatalf("scheduled failures told %+v, want failing after %d", got, FeedFailingAfter)
	}
}

// An ended poll not folded yet is the show's poll in hand, whatever it ended
// as, but only within pollInHandWindow: one whose fold keeps failing holds the
// show back that long, not the day the sweep keeps trying it (server#403).
func TestDuePodcastPollsUnfoldedPollInHand(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	lib := podcastShowsLib(t, c, ctx, 0)
	user := seedUser(t, c, ctx)
	if err := c.FollowPodcast(ctx, user, lib.ID, "Example Show", "a1b2c3d4e5f60718"); err != nil {
		t.Fatal(err)
	}
	due := func(want int, why string) {
		t.Helper()
		if refs, err := c.DuePodcastPolls(ctx, 10); err != nil || len(refs) != want {
			t.Fatalf("%s: due %+v %v, want %d", why, refs, err, want)
		}
	}
	setFinished := func(id int64, at time.Time) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, `UPDATE bridge_jobs SET finished_at=? WHERE id=?`, at.Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}
	due(1, "no poll")
	recent := now.Add(-5 * time.Minute)
	for i, state := range []string{bridge.StateDone, bridge.StateFailed, bridge.StateCancelled} {
		id := int64(100 + i)
		if _, err := c.db.ExecContext(ctx, `INSERT INTO bridge_jobs(id,kind,library_id,rel_path,state,dedupe_key,created_at,updated_at,finished_at) VALUES(?,'podcast.poll',?,'Example Show',?,'k','t','t',?)`, id, lib.ID, state, recent.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		due(0, state+" and not folded")
		setFinished(id, now.Add(-pollInHandWindow))
		due(0, state+" unfolded at the window's edge")
		// Past the scheduler's window the show is polled again, though the sweep
		// still tries the fold for bridge.PollFoldWindow.
		setFinished(id, now.Add(-pollInHandWindow-time.Second))
		due(1, state+" unfolded past the window")
		setFinished(id, recent)
		if _, err := c.db.ExecContext(ctx, `INSERT INTO podcast_folded(job_id,at) VALUES(?,?)`, id, recent.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		due(1, state+" and folded")
	}
}

// A poll whose fold lands only after the show's next poll has folded (the
// scheduler queued that one past pollInHandWindow) is stale: it is marked
// folded but moves nothing and tells nothing.
func TestPodcastStalePollFoldIgnored(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 0)
	for _, id := range []int64{300, 301} {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO bridge_jobs(id,kind,library_id,rel_path,state,dedupe_key,created_at,updated_at,finished_at) VALUES(?,'podcast.poll',?,'Example Show','done','k','t','t',?)`, id, lib.ID, now.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 301, bridge.StateDone, "")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 300, bridge.StateFailed, "gone")); err != nil {
		t.Fatal(err)
	}
	var code string
	var failures int
	var next sql.NullString
	if err := c.db.QueryRowContext(ctx, `SELECT error_code,failures,next_poll_at FROM podcast_feed_state WHERE library_id=? AND show_path='Example Show'`, lib.ID).Scan(&code, &failures, &next); err != nil || code != "" || failures != 0 || !next.Valid {
		t.Fatalf("a stale poll's fold moved the feed: code %q failures %d next %v (%v)", code, failures, next, err)
	}
	var folded bool
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM podcast_folded WHERE job_id=300)`).Scan(&folded); err != nil || !folded {
		t.Fatalf("the stale poll was left for the sweep to retry (%v)", err)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("a stale poll told %+v", got)
	}
}

// A poll that failed once it was asked to stop is stored cancelled with the
// Bridge's result (bridge.UpdateHeld) and folds as the failure it was: its
// code, its Retry-After, the run and the notice, with the result's message,
// never the cancel's reason. A cancel alone, or one naming a code that isn't
// the feed's answer (a stopped fetch reads as a timeout, or as network on a
// v0.6.0 Bridge, a stopped handler as bridge_lost: BR-CANCEL-RACE), still
// folds nothing, while a poll stored failed with network still counts.
func TestPodcastPollFailureStoredCancelledFolds(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 0)
	next := int64(0)
	fold := func(result string) []PodcastNotice {
		t.Helper()
		next++
		j := pollJob(lib.ID, next, bridge.StateCancelled, "")
		j.Result, j.Error = json.RawMessage(result), bridge.CancelReplacedRequest
		if err := c.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	for _, result := range []string{`{}`, `{"code":"timeout"}`, `{"code":"bridge_lost"}`, `{"code":"network"}`, `{"code":"dns"}`, `{"code":"tls"}`, `{"code":"write_failed"}`} {
		if got := fold(result); len(got) != 0 {
			t.Fatalf("a cancelled poll with %s told %+v", result, got)
		}
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM podcast_feed_state WHERE library_id=?`, lib.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a cancelled poll with %s folded a feed state (%d rows, %v)", result, n, err)
		}
	}
	if got := fold(`{"code":"http_429","retry_after_seconds":3600}`); len(got) != 0 {
		t.Fatalf("a first failure told %+v", got)
	}
	var code, msg string
	var failures, run int
	var due sql.NullString
	state := func() {
		t.Helper()
		if err := c.db.QueryRowContext(ctx, `SELECT error_code,error,failures,feed_failures,next_poll_at FROM podcast_feed_state WHERE library_id=?`, lib.ID).
			Scan(&code, &msg, &failures, &run, &due); err != nil {
			t.Fatal(err)
		}
	}
	state()
	if code != "http_429" || msg != "" || failures != 1 || run != 1 || due.String != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("a failure stored cancelled: code %q, error %q, failures %d, run %d, next poll %q; want http_429 backed off an hour",
			code, msg, failures, run, due.String)
	}
	// Stored failed, network counts as today: a second failure in the run.
	next++
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, next, bridge.StateFailed, "network")); err != nil {
		t.Fatal(err)
	}
	log.take()
	state()
	if code != "network" || failures != 2 || run != 2 {
		t.Fatalf("a network failure: code %q, failures %d, run %d; want network, 2 and 2", code, failures, run)
	}
	got := fold(`{"code":"gone","message":"The feed has ended."}`)
	if len(got) != 1 || got[0].Kind != PodcastFeedFailing || got[0].Code != "gone" {
		t.Fatalf("gone after a cancel told %+v, want the feed failing", got)
	}
	state()
	if code != "gone" || msg != "The feed has ended." || due.Valid {
		t.Fatalf("gone after a cancel: code %q, error %q, next poll %q; want gone and no next poll", code, msg, due.String)
	}
}

// A feed that ended (`gone`) stays ended when a follower's Check now of it
// fails another way: the scheduled polls stay stopped (no next poll, still
// `gone`), nothing is told again, and only a success brings it back.
func TestPodcastGoneFeedStaysStoppedAfterFailedCheck(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib := podcastShowsLib(t, c, ctx, 0)
	user := seedUser(t, c, ctx)
	if err := c.FollowPodcast(ctx, user, lib.ID, "Example Show", "a1b2c3d4e5f60718"); err != nil {
		t.Fatal(err)
	}
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 1, bridge.StateFailed, "gone")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if got := log.take(); len(got) != 1 || got[0].Code != "gone" {
		t.Fatalf("gone told %+v", got)
	}
	var checked string
	state := func() (code string, next sql.NullString) {
		t.Helper()
		if err := c.db.QueryRowContext(ctx, `SELECT error_code,next_poll_at,last_checked_at FROM podcast_feed_state WHERE library_id=?`, lib.ID).Scan(&code, &next, &checked); err != nil {
			t.Fatal(err)
		}
		return code, next
	}
	for i, code := range []string{"http_500", "timeout", "bridge_lost", "feed_secret_missing"} {
		if err := c.FoldPodcastJob(ctx, followerPoll(lib.ID, int64(2+i), bridge.StateFailed, code, &bridge.Requester{ID: user})); err != nil {
			t.Fatal(err)
		}
		if got, next := state(); got != "gone" || next.Valid {
			t.Fatalf("after a Check now failing with %s: code %q, next poll %q, want gone and none", code, got, next.String)
		}
		if due, err := c.DuePodcastPolls(ctx, 10); err != nil || len(due) != 0 {
			t.Fatalf("after a Check now failing with %s the scheduler would poll %+v (%v)", code, due, err)
		}
	}
	// Each check still moves the last check's time (Check now's cooldown).
	if want := now.Format(time.RFC3339); checked != want {
		t.Fatalf("last checked %s, want the failed check's %s", checked, want)
	}
	// The lost secret is the one thing told (once): it differs from `gone`.
	if got := log.take(); len(got) != 1 || got[0].Code != "feed_secret_missing" {
		t.Fatalf("failed checks of a gone feed told %+v, want the lost secret only", got)
	}
	if err := c.FoldPodcastJob(ctx, followerPoll(lib.ID, 10, bridge.StateDone, "", &bridge.Requester{ID: user})); err != nil {
		t.Fatal(err)
	}
	if got, next := state(); got != "" || !next.Valid {
		t.Fatalf("after a successful check: code %q, next poll valid %v, want back on schedule", got, next.Valid)
	}
	if got := log.take(); len(got) != 1 || got[0].Kind != PodcastFeedRecovered {
		t.Fatalf("a gone feed checked fine told %+v, want recovered", got)
	}
}

// The failing notice's event is recorded in the fold's own transaction: a
// record that fails leaves failing_announced unset (the fold fails and is
// tried again), so the next fold announces it, once, and never says "back"
// for a notice that was never recorded.
func TestPodcastFailingRecordedWithTheFlag(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib := podcastShowsLib(t, c, ctx, 0)
	var delivered []ServerEvent
	broken := true
	c.SetPodcastNotice(&PodcastListener{
		Event: func(n PodcastNotice) (string, map[string]any, string) {
			if broken {
				return "podcast_" + n.Kind, map[string]any{"bad": make(chan int)}, "" // can't be recorded
			}
			return "podcast_" + n.Kind, map[string]any{"code": n.Code}, ""
		},
		Deliver: func(_ context.Context, ev ServerEvent) { delivered = append(delivered, ev) },
	})
	announced := func() (bool, string) {
		t.Helper()
		var a bool
		var code string
		if err := c.db.QueryRowContext(ctx, `SELECT failing_announced,failing_code FROM podcast_feed_state WHERE library_id=?`, lib.ID).Scan(&a, &code); err != nil {
			t.Fatal(err)
		}
		return a, code
	}
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 1, bridge.StateDone, "")); err != nil {
		t.Fatal(err)
	}
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 2, bridge.StateFailed, "gone")); err == nil {
		t.Fatal("a fold whose event could not be recorded succeeded")
	}
	if a, code := announced(); a || code != "" || len(delivered) != 0 {
		t.Fatalf("after a failed record: announced %v %q, delivered %v", a, code, delivered)
	}
	broken = false
	if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, 2, bridge.StateFailed, "gone")); err != nil {
		t.Fatal(err)
	}
	if a, code := announced(); !a || code != "gone" || len(delivered) != 1 || delivered[0].Data["code"] != "gone" {
		t.Fatalf("the retried fold: announced %v %q, delivered %+v", a, code, delivered)
	}
	if events, _, err := c.ListServerEvents(ctx, 0, 10, ""); err != nil || len(events) != 1 || events[0].ID != delivered[0].ID {
		t.Fatalf("feed = %+v, %v; want the delivered event only", events, err)
	}
}

// An event already in the feed under its dedup key is neither recorded nor
// delivered again, checked in the write's transaction.
func TestPodcastNoticeDedupInTheTransaction(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib := podcastShowsLib(t, c, ctx, 0)
	var delivered int
	c.SetPodcastNotice(&PodcastListener{
		Event:   func(PodcastNotice) (string, map[string]any, string) { return "podcast_x", nil, "same" },
		Deliver: func(context.Context, ServerEvent) { delivered++ },
	})
	for i, state := range []string{bridge.StateFailed, bridge.StateDone} {
		if err := c.FoldPodcastJob(ctx, pollJob(lib.ID, int64(i+1), state, "gone")); err != nil {
			t.Fatal(err)
		}
	}
	if events, _, _ := c.ListServerEvents(ctx, 0, 10, ""); len(events) != 1 || delivered != 1 {
		t.Fatalf("%d events, %d delivered; want one of each", len(events), delivered)
	}
}

// New episodes are known by GUID and id, not only by date: a twin with the
// same date-only pubDate as the latest, or an episode with no date, is news;
// a known episode at a new path is not. Episodes the Bridge first saw days ago
// (an index restored from an older backup, or catching up after an outage)
// are not news either.
func TestPodcastNewEpisodesByGUID(t *testing.T) {
	c, ctx := newTestCatalog(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := listenNotices(c)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	seen := now.Add(-time.Hour).Format(time.RFC3339)
	ep := func(f *feed.File, id, title, published string) *feed.File {
		g := withEpisode(f, id, id+".mp3", title, published)
		g.Episodes[0].FirstSeen = seen
		return g
	}
	sig := 0
	index := func(f *feed.File) []PodcastNotice {
		t.Helper()
		sig++
		if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", fmt.Sprint(sig), "", "", f, nil, false); err != nil {
			t.Fatal(err)
		}
		return log.take()
	}
	titles := func(got []PodcastNotice) []string {
		if len(got) != 1 {
			return nil
		}
		return got[0].Titles
	}
	f := ep(podcastFixture(t), "1111111111111111", "Monday", "2026-11-16T00:00:00Z")
	index(f)
	f = ep(f, "2222222222222222", "Monday Twin", "2026-11-16T00:00:00Z")
	if got := index(f); !slices.Equal(titles(got), []string{"Monday Twin"}) {
		t.Fatalf("a same-date twin: %+v", got)
	}
	f = ep(f, "3333333333333333", "Undated", "")
	if got := index(f); !slices.Equal(titles(got), []string{"Undated"}) {
		t.Fatalf("an undated episode: %+v", got)
	}
	// The Bridge renamed a known episode's file: the same GUID is no news.
	moved := *f
	moved.Episodes = slices.Clone(f.Episodes)
	moved.Episodes[0].Path, moved.Episodes[0].ID = "Example Show/undated-renamed.mp3", "3333333333333334"
	if got := index(&moved); len(got) != 0 {
		t.Fatalf("a known GUID at a new path told %+v", got)
	}
	// Found days ago: a restored index or a long outage.
	old := withEpisode(&moved, "4444444444444444", "4444444444444444.mp3", "Found Days Ago", "2026-11-17T00:00:00Z")
	old.Episodes[0].FirstSeen = now.Add(-NewEpisodeWindow - time.Hour).Format(time.RFC3339)
	if got := index(old); len(got) != 0 {
		t.Fatalf("an episode first seen days ago told %+v", got)
	}
}

// A scan that read feed.json before the Bridge's rescan read and indexed a
// newer one doesn't fold it over that index, even when the Bridge wrote both
// within one second (its updated_at can't order them: the reads do). A later
// read is always folded, so a refusal never sticks: the same second's next
// write, and a feed.json restored from a backup.
func TestPodcastStaleFeedFoldRefused(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	start := now
	// readAt is f as read from disk ms after the start; every feed.json here
	// carries the same updated_at (the Bridge's stamp is whole seconds).
	readAt := func(f *feed.File, ms int) *feed.File {
		r := *f
		r.UpdatedAt, r.ReadAt = "2026-11-17T12:00:00Z", start.Add(time.Duration(ms)*time.Millisecond)
		return &r
	}
	index := func(f *feed.File, sig string, force bool) bool {
		t.Helper()
		changed, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", sig, "", "", f, nil, force)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	count := func() int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT episode_count FROM podcast_shows WHERE library_id=?`, lib.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	older := podcastFixture(t)
	newer := withEpisode(older, "1111111111111111", "new.mp3", "New", "2026-11-17T00:00:00Z")
	newest := withEpisode(newer, "2222222222222222", "newest.mp3", "Newest", "2026-11-17T00:00:01Z")
	index(readAt(older, 0), "old", false)
	base := count()
	// The scan reads the old feed.json, the Bridge writes a newer one and
	// its rescan reads and indexes it...
	now = now.Add(time.Second)
	scanRead := readAt(older, 100)
	index(readAt(newer, 200), "new", true)
	// ...then the scan commits what it read.
	if index(scanRead, "old", false) || count() != base+1 {
		t.Fatalf("a stale fold went through: %d episodes, want %d", count(), base+1)
	}
	// The Bridge's next write in the same second, read later, is folded.
	if !index(readAt(newest, 300), "newest", true) || count() != base+2 {
		t.Fatalf("the same second's next write: %d episodes, want %d", count(), base+2)
	}
	// A feed.json restored from an older backup, read after that index.
	if !index(readAt(older, 400), "old", false) || count() != base {
		t.Fatalf("a restored feed.json: %d episodes, want %d", count(), base)
	}

	// The show followed again by a new address (another show_id at the
	// path): a scan that read the old feed.json before the replacement was
	// read and indexed doesn't put the old show back.
	scanRead = readAt(newer, 500)
	replaced := readAt(newer, 600)
	replaced.ShowID = "b000000000000001"
	index(replaced, "replaced", true)
	var id string
	if index(scanRead, "new", false) {
		t.Fatal("a stale fold of the replaced show went through")
	}
	if err := c.db.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=?`, lib.ID).Scan(&id); err != nil || id != replaced.ShowID {
		t.Fatalf("show_id %q (%v), want the replacement's %q", id, err, replaced.ShowID)
	}

	// The clock set back behind the index's read: reads are folded all the
	// same, rather than refused until the clock catches up.
	now = start.Add(-time.Hour)
	if !index(readAt(older, -3600_000+100), "old", false) {
		t.Fatal("a read after the clock was set back was refused")
	}
}
