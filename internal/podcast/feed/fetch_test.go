package feed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPodcastFetchInvalidKeepsEpisode(t *testing.T) {
	raw, err := os.ReadFile("../testdata/phase2c2/feed-fetch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{"malformed", map[string]any{"generation": "wrong"}, map[string]any{"generation": -1}, map[string]any{"generation": 1, "sha256": "bad", "bytes": 0}, map[string]any{"generation": 2, "sha256": "7be5836ba147da1215098df9f7a34f33b157d07fd92dd801928c9caef17e6d2b", "bytes": 16493, "duration_ms": 4056, "fetched_at": "invalid"}} {
		file["episodes"].([]any)[0].(map[string]any)["fetch"] = bad
		changed, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(t.TempDir(), "feed.json")
		if err = os.WriteFile(filename, changed, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := ReadFile(filename, "Fixture Show", nil)
		if err != nil || len(f.Episodes) != 3 || ValidFetch(f.Episodes[0].Fetch) {
			t.Fatalf("invalid fetch dropped episode: %+v %v", f, err)
		}
	}
}
func TestPodcastEpisodeAudioAllowList(t *testing.T) {
	for _, ext := range []string{"mp3", "m4a", "m4b", "mp4", "ogg", "opus", "flac", "MP3"} {
		if !EpisodePath("Show", "Show/episode."+ext) {
			t.Fatal(ext)
		}
	}
	for _, p := range []string{"Show/x.wav", "Show/x.aac", "Show/x.mp3|m4a", "Show/x.mp3/extra", "Show/.hidden.mp3", "../x.mp3", ".show/x.mp3"} {
		if EpisodePath("Show", p) {
			t.Fatalf("accepted %s", p)
		}
	}
}
