package library

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func podcastDiskFixture(t testing.TB) (context.Context, *catalog.Catalog, *catalog.Library, *Scanner, *feed.File, string) {
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
func writePodcastFixture(t testing.TB, dest, name string) {
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

// TestPodcastFeedMemo (server#180): an episode opens again from the feed copy
// read before (allowed), and a feed.json replaced by another file at the same
// size and mtime, whose episode no longer names the pinned generation, is read
// again and refused (denied), as is the copy remembered for a show once its
// episode is gone from a new copy.
func TestPodcastFeedMemo(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	filename := filepath.Join(dir, "feed.json")
	setFeedTime(t, filename, time.Now().Add(-time.Hour)) // past the racy window
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
		if err != nil {
			t.Fatal(err)
		}
		opened.Close()
	}
	real, err := filepath.EvalSymlinks(filename) // remembered under the root's real path
	if err != nil {
		t.Fatal(err)
	}
	podcastFeeds.mu.Lock()
	remembered := podcastFeeds.feeds[real]
	podcastFeeds.mu.Unlock()
	if remembered == nil || remembered.showID != f.ShowID {
		t.Fatal("the feed copy read was not remembered")
	}
	// Another file, same size and mtime (so the pin's feed signature still
	// matches), its episode's digest changed in one hex digit.
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filename + ".tmp"
	if err := os.WriteFile(tmp, regeneratedFeed(t, filename, ep.Fetch.SHA256), 0o644); err != nil {
		t.Fatal(err)
	}
	setFeedTime(t, tmp, info.ModTime())
	if err := os.Rename(tmp, filename); err != nil {
		t.Fatal(err)
	}
	if opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		opened.Close()
		t.Fatal("served from the feed copy remembered for another file")
	}
}

// TestPodcastFeedMemoRacy (server#180): a feed.json read within the racy
// window of its mtime is not remembered, so the same file rewritten in place at
// the same size and mtime (a timestamp too coarse to tell the writes apart),
// its episode no longer naming the pinned generation, is read again and refused.
func TestPodcastFeedMemoRacy(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	filename := filepath.Join(dir, "feed.json")
	mtime := time.Now().Add(time.Hour) // never past the window, however slow the run
	setFeedTime(t, filename, mtime)
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	before, err := os.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, regeneratedFeed(t, filename, ep.Fetch.SHA256), 0o644); err != nil {
		t.Fatal(err)
	}
	setFeedTime(t, filename, mtime)
	if after, err := os.Lstat(filename); err != nil || !os.SameFile(before, after) || feedSignature(after) != feedSignature(before) {
		t.Fatalf("the rewrite was not in place at the same signature (%v)", err)
	}
	if opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		opened.Close()
		t.Fatal("served from a feed copy read within the racy window")
	}
}

// regeneratedFeed is filename's feed.json with digest, an episode's fetch
// digest, changed in its last hex digit: the same size, another generation.
func regeneratedFeed(t *testing.T, filename, digest string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	last := "0"
	if strings.HasSuffix(digest, "0") {
		last = "1"
	}
	changed := strings.Replace(string(raw), digest, digest[:len(digest)-1]+last, 1)
	if changed == string(raw) {
		t.Fatal("the fixture's digest is not in its feed")
	}
	return []byte(changed)
}

// setFeedTime sets filename's access and modification times to at.
func setFeedTime(t testing.TB, filename string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(filename, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestOpenPodcastReadsAShareFault: an episode whose look (its show's folder,
// feed.json, the publish barrier, the episode itself) fails as a share does
// that a retry may get past (EIO, ESTALE) is a look given up on (the stream's
// 503 share_not_answering), never the episode absent (ErrNotIndexable), which
// would ask the Bridge to fetch an episode the library holds. Denied the 503:
// a permission, and the file gone, stay ErrNotIndexable. Allowed: with every
// look answering the episode opens. Not parallel: it swaps podcastLstat.
func TestOpenPodcastReadsAShareFault(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
	writePodcastFixture(t, audio, "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	for _, at := range []string{dir, filepath.Join(dir, "feed.json"), filepath.Join(dir, ".audiosilo"), audio} {
		for _, c := range []struct {
			errno  syscall.Errno
			gaveUp bool
		}{{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.EACCES, false}, {syscall.ENOENT, false}} {
			if at == filepath.Join(dir, ".audiosilo") && c.errno == syscall.ENOENT {
				continue // no barrier folder: published
			}
			podcastLstat = func(p string) (os.FileInfo, error) {
				if p == at {
					return nil, &fs.PathError{Op: "lstat", Path: p, Err: c.errno}
				}
				return was(p)
			}
			opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
			if opened != nil {
				opened.Close()
				t.Errorf("%s failing %v: opened", at, c.errno)
			}
			if c.gaveUp && (!metadata.GaveUp(err) || errors.Is(err, ErrNotIndexable)) {
				t.Errorf("%s failing %v: %v, want a look given up on", at, c.errno, err)
			}
			if !c.gaveUp && !errors.Is(err, ErrNotIndexable) {
				t.Errorf("%s failing %v: %v, want ErrNotIndexable", at, c.errno, err)
			}
		}
	}
	podcastLstat = was
	opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatalf("every look answering: %v", err)
	}
	opened.Close()
}

// TestPodcastScanKeepsAShowOnAShareFault: a podcast scan whose look at a show
// (its folder, an episode's publish barrier) fails as a share does that a
// retry may get past is partial, the show and its episodes kept as indexed,
// never pruned or unpublished; one whose root listing is given up on (a share
// that stopped answering) is the library unavailable at once, nothing pruned.
// Allowed: the same scan with every look answering keeps the show. Not
// parallel: it swaps podcastLstat and the mount lookup.
func TestPodcastScanKeepsAShowOnAShareFault(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	downloaded := func() bool {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
		return err == nil && b.Episode != nil && b.Episode.Downloaded
	}
	if !downloaded() {
		t.Fatal("the fixture's episode is not indexed as downloaded")
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	for _, at := range []string{dir, filepath.Join(dir, ".audiosilo")} {
		podcastLstat = func(p string) (os.FileInfo, error) {
			if p == at {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.EIO}
			}
			return was(p)
		}
		// force: a show whose feed.json is unchanged is skipped before its barrier.
		if _, _, err := s.IndexPodcastPath(ctx, *lib, "Fixture Show"); !metadata.GaveUp(err) {
			t.Errorf("the Bridge's rescan with %s failing EIO: %v, want a look given up on", at, err)
		}
		res, err := s.Scan(ctx, *lib)
		if at == dir && (err != nil || !res.Partial) {
			t.Errorf("a scan with the show's folder failing EIO: %+v %v, want a partial run", res, err)
		}
		if !downloaded() {
			t.Errorf("with %s failing EIO the episode is no longer downloaded", at)
		}
	}
	podcastLstat = was

	release := metadata.HangMountForTests(lib.Root)
	t.Cleanup(release)
	began := time.Now()
	if _, err := s.Scan(ctx, *lib); !errors.Is(err, ErrLibraryUnavailable) || time.Since(began) > 10*time.Second {
		t.Errorf("a scan of a root on a share that stopped answering: %v after %v, want ErrLibraryUnavailable at once", err, time.Since(began))
	}
	release()
	if !downloaded() {
		t.Error("the episode is gone after a scan of a hung root")
	}
	if _, err := s.Scan(ctx, *lib); err != nil || !downloaded() {
		t.Errorf("a scan with every look answering: %v, downloaded %v", err, downloaded())
	}
}
