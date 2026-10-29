package metadata

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// probeFixtures is the in-process probe's fixture corpus
// (scripts/gen-probe-fixtures.sh).
const probeFixtures = "testdata/probe"

// probeGolden is a golden-<release>.json (cmd/probediff writes it).
type probeGolden struct {
	FFprobe string `json:"ffprobe"`
	Files   map[string]struct {
		FFprobe  *ProbeView `json:"ffprobe"`
		Error    string     `json:"ffprobe_error"`
		Fallback string     `json:"fallback"`
	} `json:"files"`
}

// fixtureFiles is every audio fixture under dir, by its slash path in dir.
func fixtureFiles(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && IsAudio(p) {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// For every release the probe knows, its answer for each fixture is that
// release's ffprobe's, recorded in golden-<release>.json, and it leaves to
// ffprobe exactly the fixtures it did when the golden was written. No ffprobe
// is needed: probe_live_test.go asks the installed one.
func TestProbeMatchesGoldens(t *testing.T) {
	t.Parallel()
	fixtures := fixtureFiles(t, probeFixtures)
	if len(fixtures) < 80 {
		t.Fatalf("%d fixtures in %s, want the whole corpus", len(fixtures), probeFixtures)
	}
	formats := map[string]bool{}
	for _, f := range fixtures {
		formats[fileFormat(f)] = true
	}
	if got := slices.Sorted(maps.Keys(formats)); !slices.Equal(got, []string{"flac", "m4a", "m4b", "mka", "mp3", "ogg", "opus"}) {
		t.Errorf("fixture formats %v, want every one the probe reads", got)
	}
	for release, ff := range knownRules {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(probeFixtures, "golden-"+release+".json"))
			if err != nil {
				t.Fatalf("no golden for release %s (scripts/gen-probe-fixtures.sh writes it): %v", release, err)
			}
			var g probeGolden
			if err := json.Unmarshal(data, &g); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(g.FFprobe, "ffprobe version "+release) && !strings.HasPrefix(g.FFprobe, "ffprobe version n"+release) {
				t.Errorf("golden-%s.json was written by %q", release, g.FFprobe)
			}
			if len(g.Files) != len(fixtures) {
				t.Errorf("golden-%s.json has %d answers for %d fixtures: regenerate it", release, len(g.Files), len(fixtures))
			}
			fallbacks := 0
			for _, name := range fixtures {
				want, ok := g.Files[name]
				if !ok {
					t.Errorf("%s: not in golden-%s.json", name, release)
					continue
				}
				got, reason, err := probeView(filepath.Join(probeFixtures, filepath.FromSlash(name)), ff)
				switch {
				case err != nil:
					t.Errorf("%s: %v", name, err)
				case want.Fallback != "":
					fallbacks++
					if reason != want.Fallback {
						t.Errorf("%s: left to ffprobe for %q, want %q", name, reason, want.Fallback)
					}
				case reason != "":
					t.Errorf("%s: left to ffprobe (%s), the golden has the probe answering", name, reason)
				case want.FFprobe == nil:
					t.Errorf("%s: answered, ffprobe failed (%s)", name, want.Error)
				default:
					for _, d := range want.FFprobe.Diff(got) {
						t.Errorf("%s: %s", name, d)
					}
				}
			}
			// The shapes ffprobe keeps: VBR MP3s with no header, a concatenated one, a
			// FLAC CUESHEET, an M4B with no moov, an MKA with no duration.
			if fallbacks < 5 || fallbacks > len(fixtures)/5 {
				t.Errorf("%d fixtures left to ffprobe, want the handful that need it", fallbacks)
			}
		})
	}
}
