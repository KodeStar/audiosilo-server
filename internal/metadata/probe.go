package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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

// ProbeTimeout is how long one ffprobe run may take.
const ProbeTimeout = 30 * time.Second

// ErrProbeTimeout is a ProbeErr when ffprobe ran out of time (killed, not a
// file it couldn't read): a slow disk or a busy one may read it given time.
var ErrProbeTimeout = errors.New("ffprobe timed out")

// ErrProbeRejected matches a ProbeErr when ffprobe read the file and rejected
// what it holds (ffmpeg's "Invalid data found when processing input"): unlike a
// time-out or a read error, the same ffprobe rejects the same bytes every time.
var ErrProbeRejected = errors.New("ffprobe rejected the file's data")

// invalidData is how ffmpeg words AVERROR_INVALIDDATA, the end of its message
// for a file it can't make sense of.
const invalidData = "Invalid data found when processing input"

// probeMessage is a failed ffprobe run's own words (probeError), and whether
// they say it rejected the file's data (ErrProbeRejected).
type probeMessage struct {
	msg      string
	rejected bool
}

func (e *probeMessage) Error() string { return e.msg }

func (e *probeMessage) Is(target error) bool { return e.rejected && target == ErrProbeRejected }

type probeResult struct {
	Duration float64
	Codec    string
	Tags     map[string]string
	Chapters []Chapter
}

// probe runs ffprobe to obtain duration, the audio codec, chapters and container
// tags. -select_streams a:0 limits -show_streams to the first audio stream so we
// read the codec that actually needs (or doesn't need) transcoding.
func probe(parent context.Context, path, ffprobePath string, timeout time.Duration) (*probeResult, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", // errors only, on stderr: what the Health page shows for a file ffprobe can't read
		"-print_format", "json",
		"-show_format",
		"-show_chapters",
		"-show_streams",
		"-select_streams", "a:0",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w after %s", ErrProbeTimeout, timeout)
		}
		return nil, probeError(err)
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

// maxProbeMessage bounds the ffprobe message kept for the Health page.
const maxProbeMessage = 300

// probeError turns a failed ffprobe run into an error carrying its own words:
// every distinct line it wrote to stderr, joined, since the specific cause ("[mov,
// mp4 @ 0x..] moov atom not found") usually comes before the generic last line
// ("x.m4b: Invalid data found when processing input"). Falls back to the exit
// status. A run whose last line (ffprobe's own error, which it writes last) ends
// in invalidData matches ErrProbeRejected; an earlier line ending so, followed by
// another error (a read failure, which may pass), does not.
func probeError(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	var parts []string
	last := ""
	for _, l := range strings.Split(string(exit.Stderr), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			last = l
		}
		// ffprobe prefixes the input path ("/srv/books/x.m4b: Invalid data..."); the
		// Health page shows the file separately.
		if i := strings.LastIndex(l, ": "); i >= 0 && strings.ContainsAny(l[:i], "/\\") {
			l = l[i+2:]
		}
		if l != "" && !slices.Contains(parts, l) {
			parts = append(parts, l)
		}
	}
	msg := strings.Join(parts, "; ")
	if msg == "" {
		return err
	}
	if len(msg) > maxProbeMessage {
		msg = strings.ToValidUTF8(msg[:maxProbeMessage], "")
	}
	return &probeMessage{msg: msg, rejected: strings.HasSuffix(last, invalidData)}
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
	if v := bookTitle(t["album"], t["title"]); v != "" {
		m.Title = v
	}
	if v := firstNonEmpty(t["album_artist"], t["artist"], t["author"]); v != "" {
		m.Author = v
	}
	// An explicit narrator tag (this one, or one the tags read) beats composer.
	if v := strings.TrimSpace(t["narrator"]); v != "" {
		m.Narrator, m.narratorExplicit = v, true
	} else if v := strings.TrimSpace(t["composer"]); v != "" && !m.narratorExplicit {
		m.Narrator = v
	}
	if v := firstNonEmpty(t["series"], t["show"], t["grouping"]); v != "" {
		m.setSeriesTag(v)
	}
	if v := firstReleaseDate(t["date"], t["year"]); v != "" {
		m.Released = v
	}
	if g := SplitGenres(t["genre"]); g != nil {
		m.Genres = g // ffprobe reads every container's genre, as for the other tags
	}
	if v := t["series-part"]; v != "" {
		if start, end, ok := ParseSeriesPosition(v); ok {
			m.SeriesIndex, m.SeriesIndexEnd = start, end // a range: an omnibus, "1-3"
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
