package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// TestPodcastChaptersReadThroughTheLibraryRoot (security): an episode's
// chapter attachment is read through the library's root, so a show folder
// swapped for a link out of the library after its checks is never followed:
// the chapters of a file outside are not read. Allowed: the show's own.
func TestPodcastChaptersReadThroughTheLibraryRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("making a link needs a privilege on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const base = "2026-10-19-1cc306d831.mp3"
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/chap-v24-reverse.chapters.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		GeneratedAt string `json:"generated_at"`
		Files       []struct {
			Size       int64 `json:"size"`
			DurationMS int64 `json:"duration_ms"`
		}
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	fetch := &feed.Fetch{Generation: 1, SHA256: "b9e2206c3fd1f9a9e9c78e9b467d888926e0396ec49760736f78705920ba8dda",
		Bytes: golden.Files[0].Size, DurationMS: golden.Files[0].DurationMS, FetchedAt: golden.GeneratedAt}
	show := filepath.Join(root, "Show")
	for _, dir := range []string{show, outside} {
		att := filepath.Join(dir, ".audiosilo")
		if err := os.MkdirAll(att, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(att, stem(base)+".chapters.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if l, err := podcastChapters(root, show, base, fetch); err != nil || l == nil || len(l.Chapters) != 3 {
		t.Fatalf("the show's own chapters: %+v, %v", l, err)
	}
	if err := os.Rename(show, show+".aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, show); err != nil {
		t.Fatal(err)
	}
	if l, err := podcastChapters(root, show, base, fetch); l != nil {
		t.Fatalf("chapters read through a link out of the library: %+v, %v", l, err)
	}
}
