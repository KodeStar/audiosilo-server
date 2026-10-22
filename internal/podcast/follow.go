package podcast

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// FollowCatalog is what following a feed by URL reads and writes.
type FollowCatalog interface {
	PodcastWholeGrant(ctx context.Context, who catalog.Caller, lib int64) (bool, error)
	PodcastShowByHash(ctx context.Context, lib int64, hash string, user int64) (*catalog.PodcastShow, error)
	PodcastState(ctx context.Context, lib int64, show string) (catalog.PodcastFeedState, error)
	FollowPodcast(ctx context.Context, user, lib int64, p, showID string) error
}

// FollowQueue is the Bridge queue following by URL may use.
type FollowQueue interface {
	Online(ctx context.Context) (*bridge.Heartbeat, error)
	Subscribe(ctx context.Context, r bridge.SubscribeRequest) (*bridge.Job, bool, error)
	WakePodcasts()
}

// Follower is following a feed by URL, the one implementation behind POST
// /podcasts/follow's feed_url and every row of an OPML import: the URL's hash
// proves access to a known show (current or previous URL), a whole-library
// grant is needed, a known show is followed at once (offline too), and only a
// new feed or a secret repair needs an online Bridge running podcast.subscribe
// and a writable mapping. Each call acts as the caller, never anyone else.
type Follower struct {
	Catalog FollowCatalog
	Queue   FollowQueue // nil: no Bridge service
	// ModuleOn is the podcasts module's live switch, rechecked per call (an
	// import's rows stop having effects once it is turned off); nil: on.
	ModuleOn func() bool
	Log      *slog.Logger
}

// FollowURL is one follow by URL.
type FollowURL struct {
	Caller    catalog.Caller
	LibraryID int64
	FeedURL   string // as sent
	// Private is the caller's own "this is a member feed" (a manual follow's
	// private:true). A URL that looks private is private whatever it says, and
	// nothing makes a private URL public.
	Private bool
	// AdoptOnly: a new subscription may not be queued, only an open one
	// adopted (bridge.ErrNoNewRequest otherwise).
	AdoptOnly bool
}

// The FollowOutcome states.
const (
	StateFollowing        = "following"         // a known show, followed now
	StateAlreadyFollowing = "already_following" // a known show the caller already followed
	StateSubscribing      = "subscribing"       // a podcast.subscribe was queued or adopted
)

// FollowOutcome is what a follow by URL did.
type FollowOutcome struct {
	State   string
	Show    *catalog.PodcastShow // following / already_following
	Job     *bridge.Job          // subscribing
	Created bool                 // subscribing: a new job (not an adopted one)
	Private bool                 // the request's effective privacy
}

// Following by URL's refusals (bridge's own pass through: ErrTooManyRequests,
// ErrLibraryNotWritable, ErrKindUnavailable, ErrPrivacyConflict,
// ErrNoNewRequest).
var (
	ErrNoWholeGrant      = errors.New("following a URL needs access to the whole library")
	ErrBridgeUnavailable = errors.New("no online Bridge runs podcast.subscribe")
	ErrModuleOff         = errors.New("the podcasts module is off")
)

// Follow follows r.FeedURL in r.LibraryID as r.Caller. The library is the
// caller's to have resolved as a podcast library.
func (f *Follower) Follow(ctx context.Context, r FollowURL) (*FollowOutcome, error) {
	if f.ModuleOn != nil && !f.ModuleOn() {
		return nil, ErrModuleOff
	}
	whole, err := f.Catalog.PodcastWholeGrant(ctx, r.Caller, r.LibraryID)
	if err != nil {
		return nil, err
	}
	if !whole {
		return nil, ErrNoWholeGrant
	}
	norm, err := feed.NormalizeStable(r.FeedURL)
	if err != nil {
		return nil, err
	}
	out := &FollowOutcome{Private: r.Private || feed.IsPrivateFeedURL(norm)}
	show, err := f.Catalog.PodcastShowByHash(ctx, r.LibraryID, feed.FeedURLHash(norm), r.Caller.ID)
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		return nil, err
	}
	showPath := ""
	if show != nil {
		// A matched private show is private, whatever the request said.
		out.Private = out.Private || show.Private
		state, err := f.Catalog.PodcastState(ctx, r.LibraryID, show.Path)
		if err != nil {
			return nil, err
		}
		if !state.SecretMissing {
			out.State = StateFollowing
			if show.Following {
				out.State = StateAlreadyFollowing
			}
			if err := f.Catalog.FollowPodcast(ctx, r.Caller.ID, r.LibraryID, show.Path, show.ShowID); err != nil {
				return nil, err
			}
			if f.Queue != nil {
				f.Queue.WakePodcasts()
			}
			show.Following = true
			out.Show = show
			return out, nil
		}
		showPath = show.Path
	}
	if f.Queue == nil {
		return nil, ErrBridgeUnavailable
	}
	h, err := f.Queue.Online(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil || !h.Offers(bridge.KindPodcastSubscribe) {
		return nil, ErrBridgeUnavailable
	}
	j, created, err := f.Queue.Subscribe(ctx, bridge.SubscribeRequest{LibraryID: r.LibraryID, User: r.Caller.ID, FeedURL: norm, Private: out.Private, ShowPath: showPath, AdoptOnly: r.AdoptOnly})
	if err != nil {
		return nil, err
	}
	out.State, out.Job, out.Created = StateSubscribing, j, created
	return out, nil
}
