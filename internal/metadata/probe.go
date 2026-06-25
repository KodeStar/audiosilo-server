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
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
	} `json:"streams"`
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
	Codec    string
	Tags     map[string]string
	Chapters []Chapter
}

// probe runs ffprobe to obtain duration, the audio codec, chapters and container
// tags. -select_streams a:0 limits -show_streams to the first audio stream so we
// read the codec that actually needs (or doesn't need) transcoding.
func probe(path, ffprobePath string) (*probeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_chapters",
		"-show_streams",
		"-select_streams", "a:0",
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
	if len(parsed.Streams) > 0 {
		res.Codec = parsed.Streams[0].CodecName
	}
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
	if p.Codec != "" {
		m.Codec = p.Codec
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

// HasFFprobe reports whether an ffprobe binary is resolvable at the given path
// (or on PATH when path is "ffprobe").
func HasFFprobe(ffprobePath string) bool {
	if ffprobePath == "" {
		return false
	}
	_, err := exec.LookPath(ffprobePath)
	return err == nil
}
