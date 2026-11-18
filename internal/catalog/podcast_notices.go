package catalog

import (
	"cmp"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// What a PodcastNotice says (notify words each as its own event kind).
const (
	PodcastEpisodesNew   = "episodes_new"
	PodcastFeedFailing   = "feed_failing"
	PodcastFeedRecovered = "feed_recovered"
)

// FeedFailingAfter is how many polls in a row the feed itself fails before
// it is announced as failing (`gone` and a lost private-feed secret are
// announced at once). NetworkFailingAfter is the same for network-class
// failures (bridge.NetworkFailure), which every show raises at once while the
// Bridge's own internet is down: a short outage there announces nothing.
const (
	FeedFailingAfter    = 3
	NetworkFailingAfter = 6
)

// NewEpisodeWindow is how recently the Bridge must have first seen an episode
// for its index to announce it (IndexPodcastShow): the Bridge's rescan follows
// its poll within minutes, while episodes first seen days ago are an index
// restored from an older backup, or catching up after a long outage, and
// were news then, not now.
const NewEpisodeWindow = 72 * time.Hour

// maxNoticeTitles bounds PodcastNotice.Titles.
const maxNoticeTitles = 5

// PodcastNotice is a podcast show's news for the admin's notifications
// (SetPodcastNotice). It names the show and carries codes and counts only:
// never a feed address, its host or an error's text, which can hold a private
// feed's credentials. A private show's notice is sent without its names
// (notify.PodcastNotice): admins who don't follow it never see it.
type PodcastNotice struct {
	Kind      string // PodcastEpisodesNew, PodcastFeedFailing or PodcastFeedRecovered
	Library   string
	LibraryID int64
	ShowPath  string
	ShowID    string
	Show      string // the show's title
	Private   bool   // podcast_shows.private
	// New episodes: the first few titles (newest first) and every new
	// episode's feed id (how many, and what a repeat announcement is known by).
	Titles     []string
	EpisodeIDs []string
	// A failing feed: the failed poll's code and the feed's failures in a row
	// (0 for a lost secret, which is not the feed's failure).
	Code     string
	Failures int
}

// newEpisodesNotice is a show's PodcastEpisodesNew notice for the episodes an
// index found new (nil for none), newest first.
func newEpisodesNotice(lib int64, library, show string, f *feed.File, news []feed.Episode) *PodcastNotice {
	if len(news) == 0 {
		return nil
	}
	slices.SortFunc(news, func(a, b feed.Episode) int {
		return cmp.Or(cmp.Compare(b.Published, a.Published), cmp.Compare(a.Path, b.Path))
	})
	n := &PodcastNotice{Kind: PodcastEpisodesNew, Library: library, LibraryID: lib, ShowPath: show, ShowID: f.ShowID,
		Show: f.Show.Title, Private: f.Private}
	for _, e := range news {
		if len(n.Titles) < maxNoticeTitles {
			n.Titles = append(n.Titles, e.Title)
		}
		n.EpisodeIDs = append(n.EpisodeIDs, e.ID)
	}
	return n
}

// feedRun is a show's run of failed polls as the notifications count it
// (podcast_feed_state.feed_failures, feed_network_failures, failing_announced
// and failing_code).
type feedRun struct {
	failures  int // the feed's own failures in a row
	network   int // of which network-class (bridge.NetworkFailure)
	announced bool
	code      string // the code the failing notice was last told with
}

// due reports whether the run has failed long enough to announce. Each kind of
// failure counts toward its own threshold and a mixed run is announced once
// the shares add up to one whole: three of the feed's answers
// (FeedFailingAfter), six network failures (NetworkFailingAfter), or, say, two
// answers and two network failures. A real fault that a timeout interrupts is
// still announced soon; only a run of network failures alone, what an outage
// at the Bridge looks like, waits for six.
func (r feedRun) due() bool {
	other := r.failures - r.network
	return other*NetworkFailingAfter+r.network*FeedFailingAfter >= FeedFailingAfter*NetworkFailingAfter
}

// poll is how a podcast.poll that failed is read against the run.
type poll struct {
	// scheduled: the scheduler's poll (bridge.PollParams.Scheduled), not a
	// follower's Check now.
	scheduled bool
	// reachable: the Bridge reached another feed since this show's previous
	// check, or checked none (bridgeReachedFeeds), so a network-class failure
	// is this feed's, not the Bridge's own internet.
	reachable bool
}

// next is the run after a podcast.poll that ended in state with code, and the
// notice kind it raises ("" for none): a success after an announcement is
// recovered; the feed's own failure counts, announced once the run is due or,
// for `gone`, at once; a lost private-feed secret is announced at once without
// counting; any other failure (the Bridge's, the library's) leaves the run as
// it was. Once announced, `gone` or a lost secret (each stops the feed for
// good) is announced again when the notice said another code. Only a
// scheduled poll's failure counts: a follower's Check now, once a minute,
// would otherwise announce a feed failing in three minutes (server#403); its
// `gone` and lost secret are still announced at once, since polling stops on
// them. A network-class failure counts only while the Bridge reaches other
// feeds (p.reachable): an outage at the Bridge fails every show at once.
func (r feedRun) next(state, code string, p poll) (feedRun, string) {
	switch {
	case state == bridge.StateDone:
		if r.announced {
			return feedRun{}, PodcastFeedRecovered
		}
		return feedRun{}, ""
	case state != bridge.StateFailed:
		return r, ""
	case code == "feed_secret_missing":
		return r.announce(code, true)
	case uncountedCheck(code, p.scheduled):
		return r, ""
	case bridge.FeedFailure(code):
		network := bridge.NetworkFailure(code)
		if network && !p.reachable {
			return r, ""
		}
		r.failures++
		if network {
			r.network++
		}
		return r.announce(code, r.due() || code == "gone")
	}
	return r, ""
}

// announce is the run once a failure with code is told when due: a run not
// announced yet is, and one already announced only for a code that stops the
// feed (`gone`, a lost secret) and that it wasn't told with.
func (r feedRun) announce(code string, due bool) (feedRun, string) {
	stops := code == "gone" || code == "feed_secret_missing"
	if !due || r.announced && (!stops || r.code == code) {
		return r, ""
	}
	r.announced, r.code = true, code
	return r, PodcastFeedFailing
}

// uncountedCheck reports whether a failed poll with code is a follower's Check
// now (not scheduled) of a feed failing by its own fault: the run doesn't count
// it, and it leaves the scheduled polls' backoff alone (FoldPodcastJob). `gone`
// always counts: polling stops on it.
func uncountedCheck(code string, scheduled bool) bool {
	return bridge.FeedFailure(code) && !scheduled && code != "gone"
}
