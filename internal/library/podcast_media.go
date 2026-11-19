package library

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// OpenPodcast pins an opened committed generation. It never probes or hashes
// during a media request, and never opens a path after the final checks. An
// episode not there (or not the generation the index pinned) is
// ErrNotIndexable, which a stream answers by fetching it again; a look that
// fails as a share does that a retry may get past (metadata.TransientFault:
// EIO, ESTALE) is a look given up on instead (podcastLookErr: the stream's 503
// share_not_answering), never the episode absent, so a share's hiccup never
// asks the Bridge for an episode the library holds.
func OpenPodcast(ctx context.Context, cat *catalog.Catalog, lib catalog.Library, rel string) (*os.File, *feed.Fetch, error) {
	show := path.Dir(rel)
	if !feed.EpisodePath(show, rel) {
		return nil, nil, ErrNotIndexable
	}
	for range 2 {
		pin, err := cat.PodcastPin(ctx, lib.ID, rel)
		if err != nil {
			return nil, nil, err
		}
		if !pin.Present || !feed.ValidFetch(pin.Fetch) || pin.Fetch.DeletedAt != "" {
			return nil, nil, ErrNotIndexable
		}
		dir, err := ContentJoin(lib.Root, show)
		if err != nil {
			return nil, nil, ErrNotIndexable
		}
		st, err := podcastLstat(filepath.Join(lib.Root, show))
		if err != nil {
			return nil, nil, podcastLookErr(err)
		}
		if !st.IsDir() {
			return nil, nil, ErrNotIndexable
		}
		filename := filepath.Join(dir, "feed.json")
		feedInfo, err := podcastLstat(filename)
		if err != nil {
			return nil, nil, podcastLookErr(err)
		}
		if feedSignature(feedInfo) != pin.FeedSig {
			return nil, nil, ErrNotIndexable
		}
		if err := podcastBarrier(dir, path.Base(rel)); err != nil {
			return nil, nil, err
		}
		current, err := podcastFeeds.read(filename, show, feedInfo)
		if err != nil {
			return nil, nil, podcastLookErr(err)
		}
		if !current.holds(rel, pin.ShowID, pin.EpisodeID, *pin.Fetch) {
			return nil, nil, ErrNotIndexable
		}
		abs := filepath.Join(dir, path.Base(rel))
		before, err := podcastLstat(abs)
		if err != nil {
			return nil, nil, podcastLookErr(err)
		}
		if !before.Mode().IsRegular() {
			return nil, nil, ErrNotIndexable
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil, nil, podcastLookErr(err)
		}
		// The final checks, after the open, each made once the one before it
		// answered: a share's fault a retry may get past on any of them is a
		// look given up on, as on the looks before.
		opened, err := f.Stat()
		var named, feedNow os.FileInfo
		if err == nil {
			named, err = podcastLstat(abs)
		}
		if err == nil {
			err = podcastBarrier(dir, path.Base(rel))
		}
		if err == nil {
			feedNow, err = podcastLstat(filename)
		}
		if err == nil && opened.Mode().IsRegular() && named.Mode().IsRegular() &&
			opened.Size() == pin.Fetch.Bytes && os.SameFile(before, opened) && os.SameFile(opened, named) && feedSignature(feedNow) == pin.FeedSig {
			return f, pin.Fetch, nil
		}
		_ = f.Close()
		if err = transientGaveUp(err); metadata.GaveUp(err) {
			return nil, nil, err
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, ErrNotIndexable
}

// podcastLstat is OpenPodcast's look at a show's folder, its feed.json, its
// publish barrier and an episode: a test injects a share's errors through it.
var podcastLstat = os.Lstat

// podcastLookErr is OpenPodcast's answer for a look that failed with err: a
// share's fault a retry may get past (metadata.TransientFault) is a look given
// up on (metadata.AsGaveUp), never the episode absent; any other
// ErrNotIndexable, the episode not there as pinned.
func podcastLookErr(err error) error {
	if err = transientGaveUp(err); metadata.GaveUp(err) {
		return err
	}
	return ErrNotIndexable
}

// OpenPodcastBounded is OpenPodcast as one metadata.Bounded call (ctx, at most
// timeout, on the episode's mount): its looks and open, on a share that
// stopped answering, are given up on (metadata.GaveUp) rather than holding the
// caller, a file opened after that closed.
func OpenPodcastBounded(ctx context.Context, cat *catalog.Catalog, lib catalog.Library, rel string, timeout time.Duration) (*os.File, *feed.Fetch, error) {
	type opened struct {
		f   *os.File
		pin *feed.Fetch
	}
	o, err := metadata.Bounded(ctx, filepath.Join(lib.Root, filepath.FromSlash(rel)), timeout, func() (opened, error) {
		f, pin, err := OpenPodcast(ctx, cat, lib, rel)
		return opened{f, pin}, err
	}, func(o opened) { _ = o.f.Close() })
	return o.f, o.pin, err
}

// The downloaded episodes of the feed.json copies OpenPodcast read last
// (server#180). Every stream request opens its episode again (a player's Range
// requests, a seek), and parsing the show's feed.json each time cost about
// 14 ms and 10 MB for a 1,000-episode show. A copy is read again once its file
// is not the one read (os.SameFile: the Bridge replaces feed.json by a rename,
// so a new copy is a new file), or its size or mtime moved; OpenPodcast's
// other checks (the signature against the pin, the publish barrier, the audio
// file's identity and size) still run on every open. A copy whose mtime is
// within feedRacyWindow of the moment its read began is not remembered (git's
// racy-clean rule): a rewrite in place, or onto a reused inode, inside the
// file system's timestamp granularity could keep the size and mtime the memo
// checks.
var podcastFeeds feedMemo

const (
	// maxFeedMemo bounds the remembered copies: one per show streamed lately.
	maxFeedMemo = 64
	// feedRacyWindow is how old a copy's mtime must be when its read begins to
	// be remembered: past the coarsest timestamps a library may sit on (FAT's
	// two seconds; a second on some network shares).
	feedRacyWindow = 2 * time.Second
)

// feedMemo is the copies remembered, by feed.json's path.
type feedMemo struct {
	mu    sync.Mutex
	feeds map[string]*feedCopy
}

// feedCopy is one feed.json copy's downloaded episodes, by path (a path names
// one episode: feed.ReadFile keeps the first).
type feedCopy struct {
	info     os.FileInfo // the file read, taken before and checked after the read
	showID   string
	episodes map[string]feedEpisode
}

// feedEpisode is a downloaded episode as its feed names it.
type feedEpisode struct {
	id    string
	fetch feed.Fetch
}

// holds reports whether the copy names episode id at rel, in show showID, with
// fetch: the generation the index pinned.
func (c *feedCopy) holds(rel, showID, id string, fetch feed.Fetch) bool {
	ep, ok := c.episodes[rel]
	return ok && c.showID == showID && ep.id == id && ep.fetch == fetch
}

// read is filename's copy (show's feed.json), remembered (once its mtime is
// past feedRacyWindow) while the file is the one read, at the size and mtime
// read; feed.ReadFile's error otherwise.
// before is filename's Lstat, taken by the caller before the call.
func (m *feedMemo) read(filename, show string, before os.FileInfo) (*feedCopy, error) {
	m.mu.Lock()
	c := m.feeds[filename]
	m.mu.Unlock()
	if c != nil && sameFeedFile(c.info, before) {
		return c, nil
	}
	// Asked before the read: a write that lands while it runs, or between it
	// and the stat below, must fall outside the mtime's granule to be seen.
	settled := time.Since(before.ModTime()) >= feedRacyWindow
	f, err := feed.ReadFile(filename, show, nil)
	if err != nil {
		return nil, err
	}
	c = &feedCopy{info: before, showID: f.ShowID, episodes: map[string]feedEpisode{}}
	for _, ep := range f.Episodes {
		if ep.Fetch != nil {
			c.episodes[ep.Path] = feedEpisode{id: ep.ID, fetch: *ep.Fetch}
		}
	}
	// Remembered only when the file read is the one stat'ed before (one
	// replaced meanwhile may have been read in either version), and its mtime
	// was past the racy window (one written since could keep it).
	if after, err := os.Lstat(filename); settled && err == nil && sameFeedFile(before, after) {
		m.mu.Lock()
		if _, held := m.feeds[filename]; m.feeds == nil || (!held && len(m.feeds) >= maxFeedMemo) {
			m.feeds = map[string]*feedCopy{}
		}
		m.feeds[filename] = c
		m.mu.Unlock()
	}
	return c, nil
}

// sameFeedFile reports whether a and b are one file at one signature (size and
// mtime: feedSignature, what the index pins).
func sameFeedFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && feedSignature(a) == feedSignature(b)
}
