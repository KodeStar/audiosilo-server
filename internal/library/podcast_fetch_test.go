package library

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func podcastDiskFixture(t *testing.T) (context.Context, *catalog.Catalog, *catalog.Library, *Scanner, *feed.File, string) {
	t.Helper()
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	root := t.TempDir()
	dir := filepath.Join(root, "Fixture Show")
	if err := os.MkdirAll(filepath.Join(dir, ".audiosilo"), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: root, Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(cat, "must-not-probe", slog.Default())
	return ctx, cat, lib, scanner, f, dir
}
func writePodcastFixture(t *testing.T, dest, name string) {
	t.Helper()
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dest, raw, 0644); err != nil {
		t.Fatal(err)
	}
}
func TestPodcastCommittedCHAPAndEviction(t *testing.T) {
	for _, golden := range []string{"chap-v23", "chap-v24-reverse"} {
		t.Run(golden, func(t *testing.T) {
			ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
			ep := &f.Episodes[0]
			// The goldens give exact bytes and timing, while feed duration is misleading.
			var chapters struct {
				GeneratedAt string `json:"generated_at"`
				Files       []struct {
					Size int64 `json:"size"`
				}
			}
			raw, err := os.ReadFile("../podcast/testdata/phase2c2/" + golden + ".chapters.json")
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &chapters); err != nil {
				t.Fatal(err)
			}
			ep.Fetch.Bytes = chapters.Files[0].Size
			ep.Fetch.FetchedAt = chapters.GeneratedAt
			if golden == "chap-v23" {
				ep.Fetch.SHA256 = "b9e2206c3fd1f9a9e9c78e9b467d888926e0396ec49760736f78705920ba8dda"
			}
			raw, _ = json.Marshal(f)
			if err = os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0644); err != nil {
				t.Fatal(err)
			}
			audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
			writePodcastFixture(t, audio, golden+".mp3")
			sidecar := filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".chapters.json")
			writePodcastFixture(t, sidecar, golden+".chapters.json")
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || b.Duration != 4.056 || b.Title != "Current generation" || b.Codec != "mp3" || b.ChaptersSource != "id3_chap" || len(b.Chapters) != 3 || b.Chapters[1].Title != "Chapter 2" || b.Chapters[1].Start != 1 || len(b.Files) != 1 || b.Generated {
				t.Fatalf("CHAP: %+v %v", b, err)
			}
			// Removing only the sidecar is noticed with feed.json untouched.
			if err = os.Remove(sidecar); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err = cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || len(b.Chapters) != 1 || b.ChaptersSource != "" {
				t.Fatalf("fallback %+v %v", b, err)
			}
			if err = os.Remove(audio); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err = cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || b.Episode.Downloaded || b.Size != 0 || b.Duration != 999 || len(b.Files) != 0 || len(b.Chapters) != 0 || b.Episode.Generation != 2 {
				t.Fatalf("eviction %+v %v", b, err)
			}
		})
	}
}
func stem(base string) string { return base[:len(base)-len(path.Ext(base))] }

func TestPodcastPublicationBoundariesFailClosed(t *testing.T) {
	// The Bridge owns journal recovery. These are the server-visible snapshots
	// after faults at each publication boundary, including failed targeted rescan.
	for _, boundary := range []string{"journal", "journal_symlink", "audio", "sidecar", "feed", "remove", "rescan_failed", "committed"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
			ep := f.Episodes[0]
			feedPath := filepath.Join(dir, "feed.json")
			newFeed, _ := os.ReadFile(feedPath)
			ep0 := f.Episodes[0]
			ep0.Fetch = nil
			f.Episodes[0] = ep0
			oldFeed, _ := json.Marshal(f)
			os.WriteFile(feedPath, oldFeed, 0644)
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".publish.json")
			writePodcastFixture(t, journal, "publish-v1.json")
			if boundary == "journal_symlink" {
				if err := os.Rename(journal, journal+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(journal+".target", journal); err != nil {
					t.Fatal(err)
				}
			}
			if boundary != "journal" {
				writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
			}
			if boundary != "journal" && boundary != "audio" {
				writePodcastFixture(t, filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".chapters.json"), "chap-v24-reverse.chapters.json")
			}
			if boundary == "feed" || boundary == "remove" || boundary == "rescan_failed" || boundary == "committed" {
				os.WriteFile(feedPath, newFeed, 0644)
			}
			if boundary == "remove" || boundary == "rescan_failed" || boundary == "committed" {
				os.Remove(journal)
			}
			if boundary != "rescan_failed" && boundary != "remove" {
				if _, _, err := s.IndexPodcastPath(ctx, *lib, ep.Path); err != nil {
					t.Fatal(err)
				}
			}
			opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
			if boundary == "committed" {
				if err != nil {
					t.Fatal(err)
				}
				opened.Close()
			} else {
				if opened != nil {
					opened.Close()
					t.Fatal("uncommitted generation served")
				}
				if err == nil {
					t.Fatal("missing barrier error")
				}
			}
			// An index-only rebuild at any barrier keeps the episode without granting presence.
			b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || (boundary != "committed" && b.Episode.Downloaded) {
				t.Fatalf("snapshot: %+v %v", b, err)
			}
		})
	}
}
func TestPodcastDescriptorSurvivesRenameAndLinksDenied(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
	writePodcastFixture(t, audio, "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, pin, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if pin.Generation != 2 {
		t.Fatal(pin)
	}
	if err = os.Rename(audio, audio+".retired"); err != nil {
		t.Skipf("OS refuses open-file rename: %v", err)
	}
	data, err := io.ReadAll(opened)
	if err != nil || int64(len(data)) != pin.Bytes {
		t.Fatal("opened generation was lost")
	}
	if err = os.Symlink(audio+".retired", audio); err != nil {
		t.Fatal(err)
	}
	if _, _, err = OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		t.Fatal("audio symlink served")
	}
	os.Remove(audio)
	os.Rename(audio+".retired", audio)
	os.Rename(filepath.Join(dir, ".audiosilo"), filepath.Join(dir, "attachments"))
	os.Symlink(filepath.Join(dir, "attachments"), filepath.Join(dir, ".audiosilo"))
	if _, _, err = OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		t.Fatal("attachment-directory symlink served")
	}
}
