package metadata

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
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
// file it couldn't read): a slow disk or a busy one may read it given time. The
// console's Health page tells a time-out from a damaged file by its text at the
// start of the stored detail (admin-ui issues-model.ts PROBE_TIMEOUT).
var ErrProbeTimeout = errors.New("ffprobe timed out")

// ErrProbeRejected matches a ProbeErr when ffprobe read the file and rejected
// what it holds (ffmpeg's "Invalid data found when processing input"), or found
// it in a format InputFormats leaves out ("Format not on whitelist"): unlike a
// time-out or a read error, the same ffprobe rejects the same bytes every time.
var ErrProbeRejected = errors.New("ffprobe rejected the file's data")

// invalidData is how ffmpeg words AVERROR_INVALIDDATA, the end of its message
// for a file it can't make sense of.
const invalidData = "Invalid data found when processing input"

// notOnWhitelist is how ffmpeg says the format it found for an input is not on
// its -format_whitelist (InputFormats), followed by the list in quotes; the
// line after it, ffprobe's own, ends in invalidArgument (AVERROR(EINVAL)).
const (
	notOnWhitelist  = "Format not on whitelist"
	invalidArgument = "Invalid argument"
)

// whitelistQuote is the quoted list after notOnWhitelist, dropped from the
// message the Health page shows.
var whitelistQuote = regexp.MustCompile(notOnWhitelist + ` '[^']*'`)

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
	// Counted: Duration is an MP3's frames counted (a VBR file with no Xing
	// or VBRI header, SRV-16), where ffprobe's is an estimate from a bitrate.
	Counted bool
}

// probe runs ffprobe on the file at path under root to obtain duration, the
// audio codec, chapters and container tags (runFFprobe).
func probe(parent context.Context, root, path, ffprobePath string, timeout time.Duration) (*probeResult, error) {
	parsed, err := runFFprobe(parent, root, path, ffprobePath, timeout)
	if err != nil {
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

// TestInputEnv names, in a test binary only, the environment variable an
// ffprobe stand-in reads the path of the file it was handed from.
const TestInputEnv = "AUDIOSILO_TEST_INPUT"

// ffprobeKillGrace is how long runFFprobe waits for an ffprobe killed at its
// time-out to end before leaving it (tests shorten it).
var ffprobeKillGrace = 5 * time.Second

// runFFprobe runs ffprobe on the file at path under root and returns its
// answer. The file is opened through root (openBlockFile, bounded as a tag
// read's open is) and handed to ffprobe opened (ToolInput), so ffprobe never
// opens the path itself: a link swapped in along it since it was checked
// can't lead ffprobe out of the library, and it reads the file
// as an audio container only (InputFormatArgs), never as a playlist of other
// files. -select_streams a:0
// limits -show_streams to the first audio stream so we read the codec that
// actually needs (or doesn't need) transcoding. One killed at its time-out
// that has not ended ffprobeKillGrace later (stuck in a read the kernel won't
// interrupt: an NFS share on macOS) is left to end on its own, counted on
// path's mount as a read given up on (await), and answers as timed out.
func runFFprobe(parent context.Context, root, path, ffprobePath string, timeout time.Duration) (*ffprobeOutput, error) {
	stuck := stuckByMount.For(path)
	if stuck.saturated() {
		// Reads from its mount hang already (a share that stopped answering):
		// ffprobe would hang there too, one more process left behind, so it is
		// not run, as readFile refuses an open there.
		return nil, fmt.Errorf("%w: %w", ErrProbeTimeout, errReadRefused)
	}
	// Marked on the read's progress for as long as it runs: it reports none
	// until it returns (Progress.Probe).
	defer probing(parent)()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	timedOut := func() error {
		if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w after %s", ErrProbeTimeout, timeout)
		}
		return nil
	}
	_, f, err := openBlockFile(ctx, root, path)
	switch {
	case errors.Is(err, errReadRefused):
		return nil, fmt.Errorf("%w: %w", ErrProbeTimeout, errReadRefused)
	case errors.Is(err, errOpenAbandoned):
		return nil, cmp.Or(timedOut(), context.Cause(ctx))
	case err != nil:
		return nil, err
	}
	input, extra, cleanup, err := ToolInput(f, root, path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// The run owns f and its input once it starts, and lets them go when
	// ffprobe ends: one left running (await) still reads them. A run that
	// never starts (its ctx ended first) leaves them to this call.
	release := func() {
		cleanup()
		_ = f.Close()
	}
	var claimed atomic.Bool // by the run or by this call, whichever is first
	defer func() {
		if claimed.CompareAndSwap(false, true) {
			release()
		}
	}()
	args := append([]string{
		"-v", "error", // errors only, on stderr: what the Health page shows for a file ffprobe can't read
		"-print_format", "json",
		"-show_format",
		"-show_chapters",
		"-show_streams",
		"-select_streams", "a:0",
	}, InputFormatArgs()...)
	cmd := exec.CommandContext(ctx, ffprobePath, append(args, input)...)
	cmd.ExtraFiles = extra
	if testing.Testing() {
		// A test's ffprobe stand-in tells the files it is handed apart by
		// their paths (TestInputEnv), as ffprobe is never given one.
		cmd.Env = append(os.Environ(), TestInputEnv+"="+path)
	}
	type ran struct {
		out []byte
		err error
	}
	wait, stop := graceAfter(ctx, ffprobeKillGrace)
	defer stop()
	r, ok := await(wait, stuck, func() ran {
		if !claimed.CompareAndSwap(false, true) {
			return ran{err: context.Cause(wait)}
		}
		defer release()
		out, err := cmd.Output()
		return ran{out, err}
	}, nil)
	out, err := r.out, r.err
	if !ok {
		err = context.Cause(ctx)
	}
	if err != nil {
		if err := timedOut(); err != nil {
			return nil, err
		}
		return nil, probeError(err)
	}
	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

// graceAfter is a context that ends grace after ctx does, or at stop.
func graceAfter(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	g, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var timer atomic.Pointer[time.Timer]
	unhook := context.AfterFunc(ctx, func() { timer.Store(time.AfterFunc(grace, cancel)) })
	return g, func() {
		unhook()
		if t := timer.Load(); t != nil {
			t.Stop()
		}
		cancel()
	}
}

// ffContextAddr is the address in FFmpeg's log prefix, "[mov,mp4 @ 0x7de824000]".
var ffContextAddr = regexp.MustCompile(` @ 0x[0-9a-fA-F]+\]`)

// maxProbeMessage bounds the ffprobe message kept for the Health page.
const maxProbeMessage = 300

// probeError turns a failed ffprobe run into an error carrying its own words:
// every distinct line it wrote to stderr, joined, since the specific cause ("[mov,
// mp4 @ 0x..] moov atom not found") usually comes before the generic last line
// ("x.m4b: Invalid data found when processing input"). Falls back to the exit
// status. A run whose last line (ffprobe's own error, which it writes last) ends
// in invalidData, or in invalidArgument right after a notOnWhitelist line (a
// format InputFormats leaves out), matches ErrProbeRejected; an earlier line
// ending so, followed by another error (a read failure, which may pass), does
// not.
func probeError(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	var parts []string
	last, before := "", ""
	for _, l := range strings.Split(string(exit.Stderr), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			last, before = l, last
		}
		// ffprobe prefixes the input path ("/srv/books/x.m4b: Invalid data..."); the
		// Health page shows the file separately.
		if i := strings.LastIndex(l, ": "); i >= 0 && strings.ContainsAny(l[:i], "/\\") {
			l = l[i+2:]
		}
		// FFmpeg names the reader that complained by its address ("[mp3 @
		// 0xcbcc3c000]"): new each run, and noise on the Health page (server#272).
		l = ffContextAddr.ReplaceAllString(l, "]")
		l = whitelistQuote.ReplaceAllString(l, notOnWhitelist)
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
	refused := strings.HasSuffix(last, invalidArgument) && strings.Contains(before, notOnWhitelist)
	return &probeMessage{msg: msg, rejected: strings.HasSuffix(last, invalidData) || refused}
}

func normalizeTags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

// applyProbe lays a probe's answer over m. The format tags it reads are
// probeTagKeys: a key read here joins that list, so the in-process probe's
// parity checks cover it.
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
	m.setTitle(t["album"], t["title"])
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
	m.setHints(t) // ffprobe names a TXXX and an MP4 freeform item by its name
	if v := t["series-part"]; v != "" {
		if start, end, ok := seriesPartTag(v); ok {
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
