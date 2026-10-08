package chapteralign

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// mythos is a real book: Stephen Fry's Mythos as one m4b with 34 broad chapters
// (testdata/mythos/local.json, ffprobe's), the community recording's 174 chapters
// (community.json, which end with an 11-minute preview this copy lacks) and every
// pause of at least 0.4 s in the audio (pauses.json, ffmpeg silencedetect at
// -40 dB), so the snap is the real file's without the audio.
type mythos struct {
	files  []File
	local  []metadata.Chapter
	remote []Remote
	pauses []Silence
}

func loadMythos(t *testing.T) mythos {
	t.Helper()
	read := func(name string, v any) {
		raw, err := os.ReadFile(filepath.Join("testdata", "mythos", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var local struct {
		Duration float64            `json:"duration"`
		Chapters []metadata.Chapter `json:"chapters"`
	}
	var m mythos
	read("local.json", &local)
	read("community.json", &m.remote)
	var pauses [][2]float64
	read("pauses.json", &pauses)
	for _, p := range pauses {
		m.pauses = append(m.pauses, Silence{Start: p[0], End: p[1]})
	}
	m.files = []File{{Path: "Mythos/Mythos.m4b", Duration: local.Duration}}
	m.local = local.Chapters
	return m
}

// recorded answers a Prober from a whole file's pauses: those overlapping the
// window, clipped to it (as ffmpeg reports a window's own).
func recorded(pauses []Silence) Prober {
	return func(_ context.Context, _ string, from, to float64) ([]Silence, error) {
		var out []Silence
		for _, p := range pauses {
			if p.End > from && p.Start < to {
				out = append(out, Silence{Start: max(p.Start, from), End: min(p.End, to)})
			}
		}
		return out, nil
	}
}

func TestMythosRefines(t *testing.T) {
	m := loadMythos(t)
	res, err := Align(context.Background(), m.files, m.local, m.remote, recorded(m.pauses))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Detail
	if res.Status != Refine {
		t.Fatalf("status %s, want refine (%+v)", res.Status, d)
	}
	// Every broad chapter but the 3 s "Outro" is a community boundary; the 140
	// community chapters between them all land on a pause; the Odyssey preview at
	// the end is not in this copy.
	if d.Anchors != 32 || d.Snapped != 140 || d.Approximate != 0 || len(res.Chapters) != 173 {
		t.Errorf("anchors %d snapped %d approximate %d chapters %d, want 32 140 0 173",
			d.Anchors, d.Snapped, d.Approximate, len(res.Chapters))
	}
	if len(d.Omitted) != 1 || d.Omitted[0] != "Preview: Chapter 1 from Odyssey" {
		t.Errorf("omitted %q", d.Omitted)
	}
	if d.Ratio < 0.999 || d.Ratio > 1.001 {
		t.Errorf("ratio %v", d.Ratio)
	}
	// Spot checks: a local chapter keeps its time exactly; a placed one sits just
	// before the speech after its pause (silencedetect's, read off the real file).
	at := map[string]float64{
		"1. Out of Chaos":            2.033,
		"2. Clash of the Titans":     5216.965,
		"Gaia’s Revenge":             771.835,
		"Pyramus and Thisbe":         46692.0,
		"Appendix 3":                 55319.461,
		"Part Two: The Toys of Zeus": 15063.063,
	}
	for _, c := range res.Chapters {
		if want, ok := at[c.Title]; ok {
			if math.Abs(c.BookOffset-want) > 1.5 {
				t.Errorf("%q at %.3f, want about %.3f", c.Title, c.BookOffset, want)
			}
			delete(at, c.Title)
		}
		if c.End <= c.Start || c.FilePath != m.files[0].Path {
			t.Errorf("chapter %d %+v", c.Index, c)
		}
	}
	if len(at) > 0 {
		t.Errorf("missing %v", at)
	}
	if last := res.Chapters[len(res.Chapters)-1]; last.End != m.files[0].Duration {
		t.Errorf("the last chapter ends at %v, not the end of the audio", last.End)
	}
}
