package podcast

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
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
