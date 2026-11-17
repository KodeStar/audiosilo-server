package library

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// OpenPodcast pins an opened committed generation. It never probes or hashes
// during a media request, and never opens a path after the final checks.
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
		st, err := os.Lstat(filepath.Join(lib.Root, show))
		if err != nil || !st.IsDir() {
			return nil, nil, ErrNotIndexable
		}
		filename := filepath.Join(dir, "feed.json")
		feedInfo, err := os.Lstat(filename)
		if err != nil || feedSignature(feedInfo) != pin.FeedSig || podcastBarrier(dir, path.Base(rel)) != nil {
			return nil, nil, ErrNotIndexable
		}
		current, err := podcastFeeds.read(filename, show, feedInfo)
		if err != nil || !current.holds(rel, pin.ShowID, pin.EpisodeID, *pin.Fetch) {
			return nil, nil, ErrNotIndexable
		}
		abs := filepath.Join(dir, path.Base(rel))
		before, err := os.Lstat(abs)
		if err != nil || !before.Mode().IsRegular() {
			return nil, nil, ErrNotIndexable
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil, nil, ErrNotIndexable
		}
		opened, err := f.Stat()
		named, nerr := os.Lstat(abs)
		if err == nil && nerr == nil && opened.Mode().IsRegular() && named.Mode().IsRegular() && opened.Size() == pin.Fetch.Bytes && os.SameFile(before, opened) && os.SameFile(opened, named) && podcastBarrier(dir, path.Base(rel)) == nil && feedSignatureMust(filename) == pin.FeedSig {
			return f, pin.Fetch, nil
		}
		_ = f.Close()
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, ErrNotIndexable
}

// The downloaded episodes of the feed.json copies OpenPodcast read last
// (server#180). Every stream request opens its episode again (a player's Range
// requests, a seek), and parsing the show's feed.json each time cost about
// 14 ms and 10 MB for a 1,000-episode show. A copy is read again once its file
// is not the one read (os.SameFile: the Bridge replaces feed.json by a rename,
// so a new copy is a new file), or its size or mtime moved; OpenPodcast's
// other checks (the signature against the pin, the publish barrier, the audio
// file's identity and size) still run on every open.
var podcastFeeds feedMemo

// maxFeedMemo bounds the remembered copies: one per show streamed lately.
const maxFeedMemo = 64

// feedMemo is the copies remembered, by feed.json's path.
type feedMemo struct {
	mu    sync.Mutex
	feeds map[string]*feedCopy
}

// feedCopy is one feed.json copy's downloaded episodes, by path.
type feedCopy struct {
	info     os.FileInfo // the file read, taken before and checked after the read
	showID   string
	episodes map[string][]feedEpisode
}

// feedEpisode is a downloaded episode as its feed names it.
type feedEpisode struct {
	id    string
	fetch feed.Fetch
}

// holds reports whether the copy names episode id at rel, in show showID, with
// fetch: the generation the index pinned.
func (c *feedCopy) holds(rel, showID, id string, fetch feed.Fetch) bool {
	if c.showID != showID {
		return false
	}
	for _, ep := range c.episodes[rel] {
		if ep.id == id && ep.fetch == fetch {
			return true
		}
	}
	return false
}

// read is filename's copy (show's feed.json), remembered while the file is
// the one read, at the size and mtime read; feed.ReadFile's error otherwise.
// before is filename's Lstat, taken by the caller before the call.
func (m *feedMemo) read(filename, show string, before os.FileInfo) (*feedCopy, error) {
	m.mu.Lock()
	c := m.feeds[filename]
	m.mu.Unlock()
	if c != nil && sameFeedFile(c.info, before) {
		return c, nil
	}
	f, err := feed.ReadFile(filename, show, nil)
	if err != nil {
		return nil, err
	}
	c = &feedCopy{info: before, showID: f.ShowID, episodes: map[string][]feedEpisode{}}
	for _, ep := range f.Episodes {
		if ep.Fetch != nil {
			c.episodes[ep.Path] = append(c.episodes[ep.Path], feedEpisode{id: ep.ID, fetch: *ep.Fetch})
		}
	}
	// Remembered only when the file read is the one stat'ed before: one
	// replaced meanwhile may have been read in either version.
	if after, err := os.Lstat(filename); err == nil && sameFeedFile(before, after) {
		m.mu.Lock()
		if m.feeds == nil || len(m.feeds) >= maxFeedMemo {
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
