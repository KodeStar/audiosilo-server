package library

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func feedSignature(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func (s *Scanner) scanPodcasts(ctx context.Context, lib catalog.Library, res *ScanResult) (*ScanResult, error) {
	entries, err := os.ReadDir(lib.Root)
	if err != nil {
		return res, ErrLibraryUnavailable
	}
	old, err := s.cat.PodcastSignatures(ctx, lib.ID)
	if err != nil {
		return res, err
	}
	if len(entries) == 0 && len(old) > 0 {
		return res, ErrLibraryUnavailable
	}
	keep := map[string]bool{}
	partial := false
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !e.IsDir() || !feed.ShowPath(e.Name()) {
			continue
		}
		n, changed, err := s.indexPodcastShow(ctx, lib, e.Name(), false)
		if errors.Is(err, ErrNotIndexable) {
			continue
		}
		if err != nil {
			partial = true
			res.Errors++
			s.log.Warn("podcast show could not be read", "library", lib.ID)
			continue
		}
		keep[e.Name()] = true
		res.Books += n
		if changed {
			if _, exists := old[e.Name()]; exists {
				res.Updated += n
			} else {
				res.Added += n
			}
		}
	}
	res.Partial = partial
	if !partial {
		if err := s.cat.PrunePodcastShows(ctx, lib.ID, keep); err != nil {
			return res, err
		}
	}
	return res, nil
}

// IndexPodcastPath is the Bridge's targeted rescan. Only show folders and an
// already indexed episode can name a show; it never discovers arbitrary audio.
func (s *Scanner) IndexPodcastPath(ctx context.Context, lib catalog.Library, p string) (string, int, error) {
	p = catalog.CleanRelPath(p)
	show := p
	if strings.Contains(p, "/") {
		b, err := s.cat.GetBookByPath(ctx, lib.ID, p)
		if err != nil || b.Episode == nil {
			return "", 0, ErrNotIndexable
		}
		show = b.Episode.ShowPath
	}
	if lib.Kind != catalog.KindPodcasts || !feed.ShowPath(show) {
		return "", 0, ErrNotIndexable
	}
	n, _, err := s.indexPodcastShow(ctx, lib, show, true)
	return show, n, err
}
func (s *Scanner) indexPodcastShow(ctx context.Context, lib catalog.Library, show string, force bool) (int, bool, error) {
	dir, err := ContentJoin(lib.Root, show)
	if err != nil {
		return 0, false, ErrNotIndexable
	}
	// A top-level show must itself be a real directory, even if a symlink would
	// remain inside the root. The Bridge owns only this one directory level.
	fi, err := os.Lstat(filepath.Join(lib.Root, show))
	if err != nil || !fi.IsDir() {
		return 0, false, ErrNotIndexable
	}
	filename := filepath.Join(dir, "feed.json")
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, ErrNotIndexable
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, ErrNotIndexable
	}
	sig := feedSignature(info)
	if !force {
		old, err := s.cat.PodcastShow(ctx, lib.ID, show, 0)
		if err == nil && old.FeedSig == sig {
			return old.EpisodeCount, false, nil
		}
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return 0, false, err
		}
	}
	f, err := feed.ReadFile(filename, show, s.log)
	if errors.Is(err, feed.ErrInvalidFeed) || errors.Is(err, feed.ErrVersion) {
		s.log.Warn("skipping invalid or unsupported podcast feed.json", "library", lib.ID)
		return 0, false, ErrNotIndexable
	}
	if err != nil {
		return 0, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, err
	}
	present := map[string]catalog.PodcastFile{}
	cover, version := "", ""
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, false, err
		}
		present[e.Name()] = catalog.PodcastFile{Size: info.Size(), MTime: info.ModTime().UnixNano()}
		if cover == "" && (e.Name() == "cover.jpg" || e.Name() == "cover.png") {
			cover = path.Join(show, e.Name())
			version = feedSignature(info)
		}
	}
	changed, err := s.cat.IndexPodcastShow(ctx, lib.ID, show, sig, cover, version, f, present, force)
	return len(f.Episodes), changed, err
}

// BrowsePodcasts reads only the index, so episodes without files remain visible
// and feed.json and artwork never become player entries.
func (s *Scanner) BrowsePodcasts(ctx context.Context, lib catalog.Library, scope catalog.Scope, user int64, rel string, offset, limit int) (*Listing, error) {
	rel = catalog.CleanRelPath(rel)
	out := &Listing{Path: rel, Entries: []Entry{}, Offset: max(offset, 0)}
	all := []Entry{}
	if rel == "" {
		shows, err := s.cat.PodcastShows(ctx, scope, user, false)
		if err != nil {
			return nil, err
		}
		for _, show := range shows {
			if scope.VisibleInBrowse(show.Path) {
				all = append(all, Entry{Name: show.Title, Path: show.Path, IsDir: true, Title: show.Title, Author: show.Author})
			}
		}
	} else {
		if !feed.ShowPath(rel) || !scope.VisibleInBrowse(rel) {
			return nil, ErrNotIndexable
		}
		show, err := s.cat.PodcastShow(ctx, lib.ID, rel, user)
		if err != nil {
			return nil, err
		}
		order := "newest"
		if show.Type == "serial" {
			order = "oldest"
		}
		cursor := ""
		for {
			page, err := s.cat.PodcastEpisodesWithin(ctx, lib.ID, rel, order, 200, cursor, scope)
			if err != nil {
				return nil, err
			}
			for _, e := range page.Episodes {
				all = append(all, Entry{Name: e.Title, Path: e.Path, IsAudio: true, IsBook: true, Title: e.Title, Author: show.Author, Duration: e.Duration, Size: e.Size})
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
	}
	out.Total = len(all)
	limit = min(max(limit, 1), 2000)
	start := min(out.Offset, len(all))
	end := min(start+limit, len(all))
	out.Entries = all[start:end]
	if end < len(all) {
		out.NextOffset = end
	}
	return out, nil
}

// ReadLibraryMarker reads only a small regular file beneath a real .audiosilo
// directory. A symlink cannot make the server attest another library's marker.
func ReadLibraryMarker(root string, id int64) (string, error) {
	dir := filepath.Join(root, metadata.AttachmentsDir)
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotIndexable
	}
	raw, err := metadata.ReadRegularFile(filepath.Join(dir, "library.json"), 4096, ErrNotIndexable, ErrNotIndexable)
	if err != nil {
		return "", ErrNotIndexable
	}
	var marker struct {
		Version   int    `json:"version"`
		LibraryID int64  `json:"library_id"`
		Nonce     string `json:"nonce"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Version != 1 || marker.LibraryID != id || !feed.ValidHex(marker.Nonce, 32) {
		return "", ErrNotIndexable
	}
	return marker.Nonce, nil
}
