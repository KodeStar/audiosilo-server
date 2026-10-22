package podcast

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func TestPhase2c2FixtureManifest(t *testing.T) {
	raw, err := os.ReadFile("testdata/phase2c2/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files map[string]struct {
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		}
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, want := range manifest.Files {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/phase2c2/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) != want.Bytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != want.SHA256 {
				t.Fatal("shared fixture changed")
			}
		})
	}
}
func TestPhase2c2ChapterGoldens(t *testing.T) {
	f, err := feed.ReadFile("testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !feed.ValidFetch(f.Episodes[0].Fetch) || !feed.ValidFetch(f.Episodes[1].Fetch) || f.Episodes[2].Fetch != nil {
		t.Fatal("fetch v1 fixture")
	}
	for _, name := range []string{"chap-v23", "chap-v24-reverse"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/phase2c2/" + name + ".chapters.json")
			if err != nil {
				t.Fatal(err)
			}
			chapters, err := metadata.ParseChapterList(raw)
			if err != nil {
				t.Fatal(err)
			}
			if chapters.From != "id3_chap" || len(chapters.Chapters) != 3 || chapters.Chapters[1].Title != "Chapter 2" || chapters.Files[0].DurationMS != 4056 {
				t.Fatalf("golden: %+v", chapters)
			}
		})
	}
}

// The 2c-3 shared fixtures are byte-identical to the planning copy: the
// manifest records every other file's length and SHA-256, and lists them all.
func TestPhase2c3FixtureManifest(t *testing.T) {
	raw, err := os.ReadFile("testdata/phase2c3/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{"manifest.json": true}
	for _, f := range manifest.Files {
		listed[f.Path] = true
		raw, err := os.ReadFile("testdata/phase2c3/" + f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != f.Bytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != f.SHA256 {
			t.Fatalf("shared fixture %s changed", f.Path)
		}
	}
	err = filepath.WalkDir("testdata/phase2c3", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("testdata/phase2c3", p)
		if !listed[filepath.ToSlash(rel)] {
			t.Errorf("%s is not in the manifest", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
