package metadata

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// ProbeView is a probe's answer as ffprobe prints it: the duration and the
// chapters' times as its "%f" strings, and the format tags applyProbe reads
// (probeTagKeys) by their lowercased names. The in-process probe's view of a
// file it answers for must equal ffprobe's (cmd/probediff, and the golden and
// live tests over testdata/probe).
type ProbeView struct {
	Duration string            `json:"duration"`
	Codec    string            `json:"codec"`
	Tags     map[string]string `json:"tags,omitempty"`
	Chapters []ProbeChapter    `json:"chapters,omitempty"`
}

// ProbeChapter is one chapter of a ProbeView.
type ProbeChapter struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Title string `json:"title"`
}

// ProbeInProcess is the in-process probe's view of the file at path as the
// ffprobe at ffprobePath would answer (its release's rules), or why it leaves
// the file to ffprobe (the view nil then); an error when the file can't be
// opened.
func ProbeInProcess(ctx context.Context, path, ffprobePath string) (*ProbeView, string, error) {
	ff := ffprobeRules(ctx, ffprobePath)
	if ff == ffNone {
		return nil, "no rules for this ffprobe's release", nil
	}
	return probeView(path, ff)
}

// ProbeRelease is the FFmpeg release ("7.1", "8.1") whose rules the probe
// follows for the ffprobe at ffprobePath; "" when the probe is off for it.
func ProbeRelease(ctx context.Context, ffprobePath string) string {
	ff := ffprobeRules(ctx, ffprobePath)
	for rel, r := range knownRules {
		if r == ff && ff != ffNone {
			return rel
		}
	}
	return ""
}

// probeView is ProbeInProcess by ff's rules.
func probeView(path string, ff ffRules) (*ProbeView, string, error) {
	p, reason, err := probeFile(path, ff)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	v := &ProbeView{Duration: ffTime(p.Duration), Codec: p.Codec, Tags: probedTags(p.Tags)}
	for _, c := range p.Chapters {
		v.Chapters = append(v.Chapters, ProbeChapter{Start: ffTime(c.Start), End: ffTime(c.End), Title: c.Title})
	}
	return v, "", nil
}

// ProbeFFprobe is ffprobe's view of the file at path, as probe reads it.
func ProbeFFprobe(ctx context.Context, path, ffprobePath string) (*ProbeView, error) {
	out, err := runFFprobe(ctx, path, ffprobePath, ProbeTimeout)
	if err != nil {
		return nil, err
	}
	v := &ProbeView{Duration: out.Format.Duration}
	if len(out.Streams) > 0 {
		v.Codec = out.Streams[0].CodecName
	}
	v.Tags = probedTags(normalizeTags(out.Format.Tags))
	for _, c := range out.Chapters {
		v.Chapters = append(v.Chapters, ProbeChapter{Start: c.StartTime, End: c.EndTime, Title: c.Tags.Title})
	}
	return v, nil
}

// probeTagKeys are the format tags applyProbe reads: the only ones that reach
// the index, so the only ones the in-process probe must name as ffprobe does
// (it reads others, such as an MP4's brands or creation_time, partly or not
// at all). A key applyProbe starts reading joins this list.
var probeTagKeys = []string{"album", "title", "album_artist", "artist", "author", "narrator", "composer",
	"series", "show", "grouping", "date", "year", "genre", "series-part"}

// probedTags is tags cut to probeTagKeys; nil when none is set.
func probedTags(tags map[string]string) map[string]string {
	var out map[string]string
	for _, k := range probeTagKeys {
		if v, ok := tags[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// ffTime is a time as ffprobe prints it.
func ffTime(s float64) string { return strconv.FormatFloat(s, 'f', 6, 64) }

// Diff lists where got differs from want, one line each (nil when they are
// equal).
func (want *ProbeView) Diff(got *ProbeView) []string {
	var out []string
	if want.Duration != got.Duration {
		out = append(out, fmt.Sprintf("duration: want %s, got %s", want.Duration, got.Duration))
	}
	if want.Codec != got.Codec {
		out = append(out, fmt.Sprintf("codec: want %q, got %q", want.Codec, got.Codec))
	}
	var keys []string
	for k := range want.Tags {
		keys = append(keys, k)
	}
	for k := range got.Tags {
		if _, ok := want.Tags[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		w, wok := want.Tags[k]
		g, gok := got.Tags[k]
		if w != g || wok != gok {
			out = append(out, fmt.Sprintf("tag %s: want %q (%t), got %q (%t)", k, w, wok, g, gok))
		}
	}
	if len(want.Chapters) != len(got.Chapters) {
		out = append(out, fmt.Sprintf("chapters: want %d, got %d", len(want.Chapters), len(got.Chapters)))
	}
	for i := range min(len(want.Chapters), len(got.Chapters)) {
		if w, g := want.Chapters[i], got.Chapters[i]; w != g {
			out = append(out, fmt.Sprintf("chapter %d: want %+v, got %+v", i, w, g))
		}
	}
	return out
}
