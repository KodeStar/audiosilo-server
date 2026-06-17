package metadata

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ffprobeOutput is the subset of ffprobe -print_format json we consume.
type ffprobeOutput struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Chapters []struct {
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
		Tags      struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

type probeResult struct {
	Duration float64
	Tags     map[string]string
	Chapters []Chapter
}

// probe runs ffprobe to obtain duration, chapters and container tags.
func probe(path, ffprobePath string) (*probeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_chapters",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	res := &probeResult{Tags: normalizeTags(parsed.Format.Tags)}
	res.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	for i, ch := range parsed.Chapters {
		start, _ := strconv.ParseFloat(ch.StartTime, 64)
		end, _ := strconv.ParseFloat(ch.EndTime, 64)
		res.Chapters = append(res.Chapters, Chapter{
			Index: i, Title: ch.Tags.Title, Start: start, End: end,
		})
	}
	return res, nil
}

func normalizeTags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

func applyProbe(m *Metadata, p *probeResult) {
	if p.Duration > 0 {
		m.Duration = p.Duration
	}
	if len(p.Chapters) > 0 {
		m.Chapters = p.Chapters
	}
	t := p.Tags
	if v := firstNonEmpty(t["album"], t["title"]); v != "" {
		m.Title = v
	}
	if v := firstNonEmpty(t["album_artist"], t["artist"], t["author"]); v != "" {
		m.Author = v
	}
	if v := firstNonEmpty(t["narrator"], t["composer"]); v != "" {
		m.Narrator = v
	}
	if v := firstNonEmpty(t["series"], t["show"], t["grouping"]); v != "" {
		m.Series = v
	}
	if v := t["series-part"]; v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m.SeriesIndex = f
		}
	}
}

// ProbeDuration returns the duration in seconds of a single media file, or 0 if
// ffprobe is unavailable or the file cannot be read. Used to measure each part
// of a multi-file book.
func ProbeDuration(path, ffprobePath string) float64 {
	if ffprobePath == "" {
		return 0
	}
	p, err := probe(path, ffprobePath)
	if err != nil {
		return 0
	}
	return p.Duration
}

// HasFFprobe reports whether an ffprobe binary is resolvable at the given path
// (or on PATH when path is "ffprobe").
func HasFFprobe(ffprobePath string) bool {
	if ffprobePath == "" {
		return false
	}
	_, err := exec.LookPath(ffprobePath)
	return err == nil
}
