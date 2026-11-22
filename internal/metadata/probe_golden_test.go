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

// countedFixtures are the fixtures that are VBR MP3s with no Xing or VBRI
// header, whose frames the probe counts (SRV-16) where ffprobe estimates their
// length from a bitrate: the length ffmpeg decodes each to (its samples over
// 44.1 kHz, `ffmpeg -i <f> -f s16le -ac 1 - | wc -c`). The header-less ones at
// 32 kbit/s, the lowest bitrate, are walked whole (mp3LowRateWalk) and keep
// the bitrate's estimate, an attached picture's 1/90000 one in craft-apic-*.
var countedFixtures = map[string]string{
	"craft-junk-vbr.mp3":     "5.355102",
	"lame-vbr-noxing.mp3":    "5.328980",
	"mp3-vbrnoxing-bare.mp3": "5.328980",
	"mp3-vbrnoxing-chap.mp3": "5.328980",
	"mp3-vbrnoxing-tags.mp3": "5.328980",
}

// For every release the probe knows, its answer for each fixture is that
// release's ffprobe's, recorded in golden-<release>.json, and it leaves to
// ffprobe exactly the fixtures it did when the golden was written. No ffprobe
// is needed: probe_live_test.go asks the installed one. A header-less VBR MP3
// (countedFixtures) is ffprobe's but for its length: its frames counted, the
// length it decodes to, never ffprobe's estimate.
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
			var g ProbeGolden
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
					if decoded, ok := countedFixtures[name]; got.Counted != ok || ok && got.Duration != decoded {
						t.Errorf("%s: duration %s counted %t, want counted %t (decoded %s, ffprobe's estimate %s)",
							name, got.Duration, got.Counted, ok, decoded, want.FFprobe.Duration)
					}
				}
			}
			// The shapes ffprobe keeps: a concatenated MP3, a FLAC CUESHEET, M4Bs with
			// no moov or a broken header, an MKA with no duration.
			if fallbacks < 5 || fallbacks > len(fixtures)/5 {
				t.Errorf("%d fixtures left to ffprobe, want the handful that need it", fallbacks)
			}
		})
	}
}
