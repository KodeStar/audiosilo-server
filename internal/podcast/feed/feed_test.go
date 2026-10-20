package feed

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFeedFixture(t *testing.T) {
	f, err := ReadFile("../testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Episodes) != 3 || f.Episodes[2].Title != "Untitled episode" || f.Episodes[1].Fetch.DurationMS != 1512345 {
		t.Fatalf("fixture: %+v", f)
	}
	// Unknown keys remain forward-compatible; bad paths never become books.
	raw, err := os.ReadFile("../testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*File)
		want   int
		denied bool
	}{
		{"valid", func(f *File) {}, 3, false},
		{"version", func(f *File) { f.Version = 2 }, 0, true},
		{"paths", func(f *File) {
			for i, p := range []string{"../outside.mp3", "Example Show/.secret.mp3", "Example Show/a/../b.mp3"} {
				f.Episodes[i].Path = p
			}
		}, 0, false},
		{"duplicate", func(f *File) { f.Episodes[1].Path = f.Episodes[0].Path }, 2, false},
		{"private", func(f *File) { f.Private = true }, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f File
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&f)
			data, _ := json.Marshal(f)
			filename := filepath.Join(t.TempDir(), "feed.json")
			if err := os.WriteFile(filename, data, 0600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			got, err := ReadFile(filename, "Example Show", slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.denied {
				if err == nil {
					t.Fatal("accepted unsupported feed")
				}
				return
			}
			if err != nil || len(got.Episodes) != tc.want {
				t.Fatalf("%v %v", got, err)
			}
			if got.Private && (got.FeedURL != "" || got.Show.Link != "" || got.Episodes[0].Enclosure.URL != "" || len(got.Episodes[0].Transcripts) != 0) {
				t.Fatal("private URLs retained")
			}
			if strings.Contains(logs.String(), "https://") {
				t.Fatal("feed data in log")
			}
		})
	}
	filename := filepath.Join(t.TempDir(), "linked.json")
	abs, _ := filepath.Abs("../testdata/feed-v1.json")
	if err := os.Symlink(abs, filename); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(filename, "Example Show", nil); err == nil {
		t.Fatal("symlink accepted")
	}
}
