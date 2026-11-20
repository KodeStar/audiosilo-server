package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// OpenPodcast pins an opened committed generation. It never probes or hashes
// during a media request, and never opens a path after the final checks. An
// episode not there (or not the generation the index pinned) is
// ErrNotIndexable, which a stream answers by fetching it again; a look that
// fails as a share does that a retry may get past (metadata.TransientFault:
// EIO, ESTALE) is a look given up on instead (podcastLookErr: the stream's 503
// share_not_answering), never the episode absent, so a share's hiccup never
// asks the Bridge for an episode the library holds; and one the share refuses
// for good (metadata.LastingFault: a permission, the device gone) is
// ErrEpisodeRefused (the stream's 404), which no fetch would get past either;
// ErrShowRefused when it is the show's folder, feed.json or publish barrier
// that is refused, which a new download of the file would not mend.
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
		dir, filename, feedInfo, err := podcastShowLooks(lib.Root, show)
		if err != nil {
			return nil, nil, err
		}
		if feedSignature(feedInfo) != pin.FeedSig {
			return nil, nil, ErrNotIndexable
		}
		if err := podcastBarrier(podcastLstat, dir, path.Base(rel)); err != nil {
			return nil, nil, err
		}
		current, err := podcastFeeds.read(lib.Root, filename, show, feedInfo)
		if err != nil {
			return nil, nil, podcastShowLookErr(err)
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
		// Opened through the library's root, as every library file is: a
		// show folder swapped for a link out of the library since its look
		// above is refused, never followed.
		f, err := metadata.OpenBelow(lib.Root, abs)
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
			err = podcastBarrier(podcastLstat, dir, path.Base(rel))
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

// podcastShowLooks is OpenPodcast's looks at the folder of show, under root,
// above any episode's file: the join, the folder and its feed.json, which
// ShowRefused makes too. dir is the folder joined, filename its feed.json and
// feedInfo that file's Lstat; err is podcastShowLookErr's for a look that
// failed (a lasting refusal ErrShowRefused), ErrNotIndexable for a show that
// is no folder.
func podcastShowLooks(root, show string) (dir, filename string, feedInfo os.FileInfo, err error) {
	if dir, err = ContentJoin(root, show); err != nil {
		// The join's look a share failed is never the episode absent; a
		// link out of the root, a name no show has, are.
		return "", "", nil, podcastShowLookErr(err)
	}
	st, err := podcastLstat(filepath.Join(root, show))
	if err != nil {
		return "", "", nil, podcastShowLookErr(err)
	}
	if !st.IsDir() {
		return "", "", nil, ErrNotIndexable
	}
	filename = filepath.Join(dir, "feed.json")
	if feedInfo, err = podcastLstat(filename); err != nil {
		return "", "", nil, podcastShowLookErr(err)
	}
	return dir, filename, feedInfo, nil
}

// ShowRefused is ErrShowRefused (with the share's error) when the share
// refuses for good the show folder of the episode at rel in lib, its
// feed.json or the episode's publish barrier: OpenPodcast's looks above the
// episode's file, made whatever the index holds of the episode (one not
// downloaded yet among them), as one metadata.Bounded call (ctx, at most
// timeout). The Bridge writes only the episode's file, which would sit behind
// the same refusal, so a fetch would download the whole episode again for
// nothing: the fetch routes answer it 409 show_unreadable, the stream its
// 404, and the scheduler and the prefetch pass the show over. nil for any
// other answer (no such folder yet, a share that stopped answering): a fetch
// may still help there.
func ShowRefused(ctx context.Context, lib catalog.Library, rel string, timeout time.Duration) error {
	show := path.Dir(rel)
	if !feed.EpisodePath(show, rel) {
		return nil
	}
	_, err := metadata.Bounded(ctx, filepath.Join(lib.Root, filepath.FromSlash(show)), timeout, func() (struct{}, error) {
		dir, filename, _, err := podcastShowLooks(lib.Root, show)
		if err != nil {
			return struct{}{}, err
		}
		if err = podcastBarrier(podcastLstat, dir, path.Base(rel)); err != nil {
			return struct{}{}, err
		}
		// Opened, not read: a refusal shows at the open, and a check made
		// on every fetch ask and scheduler pass parses no feed.
		f, err := metadata.OpenBelow(lib.Root, filename)
		if err != nil {
			return struct{}{}, podcastShowLookErr(err)
		}
		return struct{}{}, f.Close()
	}, nil)
	if errors.Is(err, ErrShowRefused) {
		return err
	}
	return nil
}

// podcastLstat is OpenPodcast's and a scan's (podcastFolderSig,
// podcastBarrier) look at a show's folder, its feed.json, its publish barrier
// and an episode: a test injects a share's errors through it.
var podcastLstat = os.Lstat

// podcastLookErr is OpenPodcast's answer for a look at the episode's own file
// that failed with err: a share's fault a retry may get past
// (metadata.TransientFault) is a look given up on (metadata.AsGaveUp), never
// the episode absent; a refusal a retry would only meet again
// (metadata.LastingFault) ErrEpisodeRefused, with err; any other
// ErrNotIndexable, the episode not there as pinned.
func podcastLookErr(err error) error { return lookErrAs(err, ErrEpisodeRefused) }

// podcastShowLookErr is podcastLookErr for a look above the episode's file
// (the show's folder, its feed.json, the publish barrier), whose lasting
// refusal is ErrShowRefused.
func podcastShowLookErr(err error) error { return lookErrAs(err, ErrShowRefused) }

// lookErrAs is podcastLookErr with refused the answer for a lasting refusal.
func lookErrAs(err, refused error) error {
	if err = transientGaveUp(err); metadata.GaveUp(err) {
		return err
	}
	if metadata.LastingFault(err) {
		return fmt.Errorf("%w: %w", refused, err)
	}
	return ErrNotIndexable
}

// ErrEpisodeRefused is OpenPodcast's for an episode whose show folder,
// feed.json, publish barrier or file the share refuses for good (a
// permission, the device gone: metadata.LastingFault): the library holds it,
// so a stream answers 404 rather than asking the Bridge to fetch it again,
// which would meet the same refusal. It is ErrNotIndexable too (errors.Is),
// so a scan, and every caller that only tells an episode there from one not,
// reads it as before.
var ErrEpisodeRefused = fmt.Errorf("%w: the share refuses the episode", ErrNotIndexable)

// ErrShowRefused is ErrEpisodeRefused for a refusal above the episode's file:
// its show folder, feed.json or publish barrier (podcastShowLookErr). A new
// download writes only the file, so it would sit behind the same refusal: a
// fetch asked for by name is refused up front (the API's 409
// show_unreadable), never queued to download the whole episode again on every
// ask. It is ErrEpisodeRefused (and so ErrNotIndexable) too, so the stream's
// 404 and a scan read it as before.
var ErrShowRefused = fmt.Errorf("%w: the share refuses the show's folder", ErrEpisodeRefused)

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
// checks. Nor is a copy whose episodes were re-rooted against the folder's
// listing (feed.File.Rerooted): a file added since would not be in it.
var podcastFeeds = feedMemo{feeds: store.GenMap[string, *feedCopy]{Max: maxFeedMemo}}

const (
	// maxFeedMemo bounds the remembered copies: one per show streamed lately.
	maxFeedMemo = 64
	// feedRacyWindow is how old a copy's mtime must be when its read begins to
	// be remembered: past the coarsest timestamps a library may sit on (FAT's
	// two seconds; a second on some network shares).
	feedRacyWindow = 2 * time.Second
)

// feedMemo is the copies remembered, by feed.json's path, at one generation
// (feedMemoGen): a copy is current while its file is the one read (holds),
// never by the index's content generation, which a write to any library moves.
type feedMemo struct {
	feeds store.GenMap[string, *feedCopy]
}

// feedMemoGen is the one generation feedMemo puts and gets at.
const feedMemoGen = 0

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

// read is filename's copy (show's feed.json, under the library root root and
// read through it), remembered (once its mtime is past feedRacyWindow) while
// the file is the one read, at the size and mtime read; feed.ReadFile's error
// otherwise.
// before is filename's Lstat, taken by the caller before the call.
func (m *feedMemo) read(root, filename, show string, before os.FileInfo) (*feedCopy, error) {
	c, ok := m.feeds.Get(feedMemoGen, filename)
	if ok && sameFeedFile(c.info, before) {
		return c, nil
	}
	// Asked before the read: a write that lands while it runs, or between it
	// and the stat below, must fall outside the mtime's granule to be seen.
	settled := time.Since(before.ModTime()) >= feedRacyWindow
	f, err := feed.ReadFile(root, filename, show, nil)
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
	// replaced meanwhile may have been read in either version), its mtime
	// was past the racy window (one written since could keep it), and its
	// episodes were read from it alone: a folder renamed by hand lists its
	// episodes against the disk too (feed.File.Rerooted), which the file's
	// identity doesn't vouch for.
	if after, err := os.Lstat(filename); settled && !f.Rerooted && err == nil && sameFeedFile(before, after) {
		m.feeds.Put(feedMemoGen, filename, c)
	}
	return c, nil
}

// sameFeedFile reports whether a and b are one file at one signature (size and
// mtime: feedSignature, what the index pins).
func sameFeedFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && feedSignature(a) == feedSignature(b)
}
