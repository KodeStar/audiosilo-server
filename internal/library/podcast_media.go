package library

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"

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
		if feedSignatureMust(filename) != pin.FeedSig || podcastBarrier(dir, path.Base(rel)) != nil {
			return nil, nil, ErrNotIndexable
		}
		current, err := feed.ReadFile(filename, show, nil)
		if err != nil {
			return nil, nil, ErrNotIndexable
		}
		matches := false
		for _, ep := range current.Episodes {
			if ep.Path == rel && ep.ID == pin.EpisodeID && current.ShowID == pin.ShowID && reflect.DeepEqual(ep.Fetch, pin.Fetch) {
				matches = true
				break
			}
		}
		if !matches {
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
