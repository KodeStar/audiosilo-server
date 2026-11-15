package catalog

import (
	"cmp"
	"slices"

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

// maxNoticeTitles bounds PodcastNotice.Titles.
const maxNoticeTitles = 5

// PodcastNotice is a podcast show's news for the admin's notifications
// (SetPodcastNotice). It names the show and carries codes and counts only:
// never a feed address, its host or an error's text, which can hold a private
// feed's credentials.
type PodcastNotice struct {
	Kind      string // PodcastEpisodesNew, PodcastFeedFailing or PodcastFeedRecovered
	Library   string
	LibraryID int64
	ShowPath  string
	ShowID    string
	Show      string // the show's title
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
		Show: f.Show.Title}
	for _, e := range news {
		if len(n.Titles) < maxNoticeTitles {
			n.Titles = append(n.Titles, e.Title)
		}
		n.EpisodeIDs = append(n.EpisodeIDs, e.ID)
	}
	return n
}

// feedRun is a show's run of failed polls as the notifications count it
// (podcast_feed_state.feed_failures, feed_network_failures and
// failing_announced).
type feedRun struct {
	failures  int // the feed's own failures in a row
	network   int // of which network-class (bridge.NetworkFailure)
	announced bool
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

// next is the run after a podcast.poll that ended in state with code, and the
// notice kind it raises ("" for none): a success after an announcement is
// recovered; the feed's own failure counts, announced once the run is due or,
// for `gone`, at once; a lost private-feed secret is announced at once without
// counting; any other failure (the Bridge's, the library's) leaves the run as
// it was.
func (r feedRun) next(state, code string) (feedRun, string) {
	switch {
	case state == bridge.StateDone:
		if r.announced {
			return feedRun{}, PodcastFeedRecovered
		}
		return feedRun{}, ""
	case state != bridge.StateFailed:
		return r, ""
	case code == "feed_secret_missing":
		if r.announced {
			return r, ""
		}
		r.announced = true
		return r, PodcastFeedFailing
	case bridge.FeedFailure(code):
		r.failures++
		if bridge.NetworkFailure(code) {
			r.network++
		}
		if r.announced || !r.due() && code != "gone" {
			return r, ""
		}
		r.announced = true
		return r, PodcastFeedFailing
	}
	return r, ""
}
