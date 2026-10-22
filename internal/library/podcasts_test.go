package library

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestPodcastScanner(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	root := t.TempDir()
	dir := filepath.Join(root, "Example Show")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../podcast/testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: root, Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "a-probe-that-must-never-run", slog.Default())
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Books != 3 || res.Added != 3 {
		t.Fatalf("scan: %+v %v", res, err)
	}
	page, err := cat.PodcastEpisodesWithin(ctx, lib.ID, "Example Show", "newest", 2, "", catalog.Scope{LibraryID: lib.ID, AllowAll: true})
	if err != nil || len(page.Episodes) != 2 || page.NextCursor == nil || page.Episodes[0].Title != "Episode Three" || page.Episodes[1].DurationSource != "feed" {
		t.Fatalf("episodes: %+v %v", page, err)
	}
	tail, err := cat.PodcastEpisodesWithin(ctx, lib.ID, "Example Show", "newest", 2, *page.NextCursor, catalog.Scope{LibraryID: lib.ID, AllowAll: true})
	if err != nil || len(tail.Episodes) != 1 || tail.Episodes[0].Title != "Untitled episode" || !tail.Episodes[0].Removed || tail.Episodes[0].Published != nil {
		t.Fatalf("tail %+v %v", tail, err)
	}
	res, err = s.Scan(ctx, *lib)
	if err != nil || res.Added != 0 || res.Updated != 0 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
	var f feed.File
	_ = json.Unmarshal(raw, &f)
	f.Episodes[1].Fetch.Bytes = 4
	updated, _ := json.Marshal(f)
	if err := os.WriteFile(filename, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f.Episodes[1].Path)), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	show, n, err := s.IndexPodcastPath(ctx, *lib, "Example Show")
	if err != nil || show != "Example Show" || n != 3 {
		t.Fatalf("rescan %s %d %v", show, n, err)
	}
	b, err := cat.GetBookByPath(ctx, lib.ID, f.Episodes[1].Path)
	if err != nil || b.Episode == nil || !b.Episode.Downloaded || b.Episode.DurationSource != "file" || b.Duration != 1512.345 {
		t.Fatalf("file duration: %+v %v", b, err)
	}
	if _, err := s.IndexPath(ctx, *lib, f.Episodes[1].Path); !errors.Is(err, ErrNotIndexable) {
		t.Fatalf("disk fallback %v", err)
	}
	scope := catalog.Scope{LibraryID: lib.ID, AllowAll: true}
	listing, err := s.BrowsePodcasts(ctx, *lib, scope, 0, "Example Show", 0, 200)
	if err != nil || listing.Total != 3 || listing.Entries[0].Name != "Episode Three" {
		t.Fatalf("browse %+v %v", listing, err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	sigs, err := cat.PodcastSignatures(ctx, lib.ID)
	if err != nil || len(sigs) != 0 {
		t.Fatalf("prune %+v %v", sigs, err)
	}
}
func TestPodcastMarker(t *testing.T) {
	for _, test := range []string{"valid", "wrong_id", "symlink_dir", "symlink_file", "oversize", "bad_nonce"} {
		t.Run(test, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".audiosilo")
			_ = os.Mkdir(dir, 0700)
			filename := filepath.Join(dir, "library.json")
			data := []byte(`{"version":1,"library_id":3,"nonce":"0123456789abcdef0123456789abcdef"}`)
			switch test {
			case "wrong_id":
				data = []byte(`{"version":1,"library_id":2,"nonce":"0123456789abcdef0123456789abcdef"}`)
			case "bad_nonce":
				data = []byte(`{"version":1,"library_id":3,"nonce":"UPPERCASE"}`)
			case "oversize":
				data = make([]byte, 4097)
			}
			_ = os.WriteFile(filename, data, 0600)
			if test == "symlink_file" {
				target := filepath.Join(t.TempDir(), "marker")
				_ = os.Rename(filename, target)
				_ = os.Symlink(target, filename)
			}
			if test == "symlink_dir" {
				target := filepath.Join(t.TempDir(), "other")
				_ = os.Rename(dir, target)
				_ = os.Symlink(target, dir)
			}
			_, err := ReadLibraryMarker(root, 3)
			if (err == nil) != (test == "valid") {
				t.Fatalf("marker %v", err)
			}
		})
	}
}

func TestPodcastScannerRejectsInvalidAndLinkedFeeds(t *testing.T) {
	for _, tc := range []string{"version2", "symlink", "badpaths"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			cat := catalog.New(storetest.Open(t), time.Now)
			root := t.TempDir()
			dir := filepath.Join(root, "Example Show")
			_ = os.Mkdir(dir, 0700)
			raw, err := os.ReadFile("../podcast/testdata/feed-v1.json")
			if err != nil {
				t.Fatal(err)
			}
			var f feed.File
			_ = json.Unmarshal(raw, &f)
			want := 0
			switch tc {
			case "version2":
				f.Version = 2
			case "badpaths":
				f.Episodes[0].Path = "Example Show/../escape.mp3"
				f.Episodes[1].Path = "Example Show/.secret.m4a"
				want = 1
			}
			data, _ := json.Marshal(f)
			filename := filepath.Join(dir, "feed.json")
			_ = os.WriteFile(filename, data, 0600)
			if tc == "symlink" {
				target := filepath.Join(t.TempDir(), "feed.json")
				_ = os.Rename(filename, target)
				_ = os.Symlink(target, filename)
			}
			lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: root, Kind: catalog.KindPodcasts})
			if err != nil {
				t.Fatal(err)
			}
			s := NewScanner(cat, "a-probe-that-must-never-run", slog.Default())
			res, err := s.Scan(ctx, *lib)
			if err != nil || res.Books != want {
				t.Fatalf("scan %+v %v", res, err)
			}
		})
	}
}
