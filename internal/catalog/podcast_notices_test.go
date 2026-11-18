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

// A scan that read an older feed.json before the Bridge's rescan indexed a
// newer one doesn't fold it over that index; one read after it (a feed.json
// restored from a backup) is indexed.
func TestPodcastStaleFeedFoldRefused(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	older := podcastFixture(t)
	newer := withEpisode(older, "1111111111111111", "new.mp3", "New", "2026-11-17T00:00:00Z")
	newer.UpdatedAt = "2026-11-17T01:00:00Z"
	count := func() int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT episode_count FROM podcast_shows WHERE library_id=?`, lib.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "old", "", "", older, nil, false); err != nil {
		t.Fatal(err)
	}
	// The scan reads the old feed.json...
	scanRead := *older
	scanRead.ReadAt = now.Add(time.Minute)
	// ...the Bridge writes a newer one and its rescan indexes it...
	now = now.Add(2 * time.Minute)
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "new", "", "", newer, nil, true); err != nil {
		t.Fatal(err)
	}
	want := count()
	// ...then the scan commits what it read.
	if changed, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "old", "", "", &scanRead, nil, false); err != nil || changed || count() != want {
		t.Fatalf("a stale fold: changed %v, %v, %d episodes, want %d", changed, err, count(), want)
	}
	// A feed.json restored from an older backup, read after that index.
	restored := *older
	restored.ReadAt = now.Add(time.Minute)
	if changed, err := c.IndexPodcastShow(ctx, lib.ID, "Example Show", "old", "", "", &restored, nil, false); err != nil || !changed || count() != want-1 {
		t.Fatalf("a restored feed.json: changed %v, %v, %d episodes, want %d", changed, err, count(), want-1)
	}
}
