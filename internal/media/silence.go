package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// SilenceTimeout bounds opening the file and the decode allowance per window.
const SilenceTimeout = 30 * time.Second

// DetectSilences seeks to all of one file's windows in one ffmpeg process.
// The concat input is a server-written list of excerpts from the same opened
// file. Each excerpt allows only audio demuxers. ffmpeg reads the excerpts in
// order, with one excerpt open at a time even on a book with many chapters.
// An ffmpeg older than 5.0 runs once a window instead (detectEachWindow).
func DetectSilences(ctx context.Context, ffmpegPath, root, absPath string, windows []metadata.TimeRange) ([][]metadata.TimeRange, error) {
	out := make([][]metadata.TimeRange, len(windows))
	offsets := make([]float64, len(windows)+1)
	for i, w := range windows {
		offsets[i+1] = offsets[i] + max(0, w.End-w.Start)
	}
	end := offsets[len(windows)]
	if end == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(windows))*SilenceTimeout)
	defer cancel()
	f, _, err := metadata.OpenRegularBelowBounded(ctx, root, absPath, SilenceTimeout)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	input, extra, cleanup, err := metadata.ToolInput(ctx, f, root, absPath, SilenceTimeout)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// ffconcat is line-oriented. Quote the held input's name as a literal
	// file URL, including any quote in its private directory or Windows path.
	if strings.ContainsAny(input, "\r\n") {
		return nil, errors.New("line break in ffmpeg input name")
	}
	pauses, reached, err := detectExcerpts(ctx, ffmpegPath, input, extra, windows, end)
	if errors.Is(err, errConcatOptions) {
		return detectEachWindow(ctx, ffmpegPath, input, extra, windows)
	}
	if err != nil {
		return nil, err
	}
	// The detector sees a single timeline. A pause can run across a cut:
	// give each window only its own part, back in seconds from the file start.
	for i, w := range windows {
		if offsets[i+1] > reached {
			// Read in part or not at all when the budget ran out: no answer,
			// as for a window that failed, rather than a shorter pause than
			// the one the whole window holds.
			break
		}
		for len(pauses) > 0 && pauses[0].End <= offsets[i] {
			pauses = pauses[1:]
		}
		for _, pause := range pauses {
			if pause.Start >= offsets[i+1] {
				break
			}
			start, end := max(pause.Start, offsets[i]), min(pause.End, offsets[i+1])
			if start < end {
				out[i] = append(out[i], metadata.TimeRange{Start: w.Start + (start - offsets[i]), End: w.Start + (end - offsets[i])})
			}
		}
	}
	return out, nil
}

// silenceFilter is the pause a chapter break makes: at least 0.4 s under
// -40 dB (chapteralign's minPause).
const silenceFilter = "silencedetect=noise=-40dB:d=0.4"

var (
	// errConcatOptions is an ffmpeg older than 5.0: its concat lists take no
	// per-file option, so no excerpt could be held to the audio demuxers.
	errConcatOptions = errors.New("ffmpeg's concat lists take no options")
	// errNoTimeline is a run whose file header never came, so the list of
	// excerpts was never sent.
	errNoTimeline = errors.New("ffmpeg did not report the audio timeline")
)

// detectExcerpts reads the container's start timestamp from ffmpeg's first
// input header. The concat demuxer's inpoints use container timestamps, while
// chapter offsets and ffmpeg's ordinary -ss are relative to the start. It
// returns the pauses on the concatenated timeline and how far that timeline
// was read: end, or less when the budget ran out.
func detectExcerpts(ctx context.Context, ffmpeg, input string, extra []*os.File, windows []metadata.TimeRange, end float64) ([]metadata.TimeRange, float64, error) {
	args := append([]string{"-nostdin", "-hide_banner", "-nostats", "-loglevel", "info"}, metadata.InputFormatArgs()...)
	args = append(args, "-i", input, "-f", "concat", "-safe", "0", "-protocol_whitelist", "file,pipe",
		"-segment_time_metadata", "1", "-i", "pipe:0", "-copyts", "-map", "1:a:0",
		"-af", "aselect=concatdec_select,"+silenceFilter, "-f", "null", "-")
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.ExtraFiles = extra
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, 0, err
	}
	defer in.Close()
	log, err := cmd.StderrPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	// ffmpeg waits for the list on stdin once it has the header: one that
	// never comes would hold the run for the whole budget of every window.
	header := time.AfterFunc(SilenceTimeout, func() { cancel(errNoTimeline) })
	defer header.Stop()
	var wrote chan error
	var pauses silenceLog
	var last string
	tooOld := false
	sc := bufio.NewScanner(log)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	for sc.Scan() {
		last = sc.Text()
		if wrote == nil && strings.HasPrefix(last, "  Duration:") {
			header.Stop()
			start, _ := silenceField(last, "start: ") // omitted when unknown
			wrote = make(chan error, 1)
			// Keep draining stderr while ffmpeg reads a long list on stdin.
			go func() {
				err := writeExcerpts(in, input, windows, start)
				_ = in.Close()
				wrote <- err
			}()
		}
		if strings.HasPrefix(last, "[silencedetect") || strings.HasPrefix(last, "[Parsed_silencedetect_") {
			pauses.add(last)
		}
		tooOld = tooOld || strings.Contains(last, "unknown keyword 'option'")
	}
	if sc.Err() != nil {
		cancel(nil)
	}
	runErr := cmd.Wait()
	if wrote != nil {
		if err := <-wrote; err != nil && runErr == nil {
			return nil, 0, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if runErr != nil && ctx.Err() == nil {
		if tooOld {
			return nil, 0, errConcatOptions
		}
		return nil, 0, fmt.Errorf("ffmpeg silencedetect: %w: %s", runErr, last)
	}
	if wrote == nil {
		if ctx.Err() != nil {
			return nil, 0, context.Cause(ctx)
		}
		return nil, 0, errNoTimeline
	}
	if runErr != nil {
		if len(pauses.closed) == 0 {
			return nil, 0, context.Cause(ctx)
		}
		// The probe budget ran out: keep the windows read whole. A pause still
		// open could end anywhere in the unread audio.
		end = pauses.seen()
	}
	return pauses.finish(end), end, nil
}

// detectEachWindow is DetectSilences on an ffmpeg older than 5.0: one run a
// window, seeking with -ss, held to the audio demuxers as an input option. A
// budget that runs out leaves that window and the rest unanswered.
func detectEachWindow(ctx context.Context, ffmpeg, input string, extra []*os.File, windows []metadata.TimeRange) ([][]metadata.TimeRange, error) {
	out := make([][]metadata.TimeRange, len(windows))
	answered := false
	for i, w := range windows {
		if w.End <= w.Start {
			continue
		}
		args := append([]string{"-nostdin", "-hide_banner", "-nostats",
			"-ss", strconv.FormatFloat(w.Start, 'f', 3, 64),
			"-t", strconv.FormatFloat(w.End-w.Start, 'f', 3, 64),
		}, metadata.InputFormatArgs()...)
		args = append(args, "-i", input, "-vn", "-af", silenceFilter, "-f", "null", "-")
		cmd := exec.CommandContext(ctx, ffmpeg, args...)
		cmd.ExtraFiles = extra
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			switch {
			case ctx.Err() == nil:
				log := strings.TrimSpace(stderr.String())
				return nil, fmt.Errorf("ffmpeg silencedetect: %w: %s", err, log[strings.LastIndexByte(log, '\n')+1:])
			case answered:
				return out, nil
			default:
				return nil, ctx.Err()
			}
		}
		answered = true
		var pauses silenceLog
		for line := range strings.SplitSeq(stderr.String(), "\n") {
			pauses.add(line)
		}
		for _, p := range pauses.finish(w.End - w.Start) {
			out[i] = append(out[i], metadata.TimeRange{Start: w.Start + p.Start, End: w.Start + p.End})
		}
	}
	return out, nil
}

func writeExcerpts(dst io.Writer, input string, windows []metadata.TimeRange, start float64) error {
	file := strings.ReplaceAll(filepath.ToSlash(input), "'", "'\\''")
	w := bufio.NewWriter(dst)
	for _, window := range windows {
		if window.End > window.Start {
			_, err := fmt.Fprintf(w, "file 'file:%s'\noption format_whitelist %s\ninpoint %.6f\noutpoint %.6f\nduration %.6f\n",
				file, metadata.InputFormats, start+window.Start, start+window.End, window.End-window.Start)
			if err != nil {
				return err
			}
		}
	}
	return w.Flush()
}

// silenceLog holds the pauses reported on ffmpeg's concatenated timeline.
type silenceLog struct {
	closed []metadata.TimeRange
	start  float64
	open   bool
}

func (l *silenceLog) add(line string) {
	if v, ok := silenceField(line, "silence_start: "); ok {
		l.start, l.open = max(v, 0), true
	} else if v, ok := silenceField(line, "silence_end: "); ok {
		l.closed = append(l.closed, metadata.TimeRange{Start: l.start, End: v})
		l.start, l.open = 0, false
	}
}

// seen is how far the log shows ffmpeg read: a pause still open's start, else
// the last pause's end.
func (l *silenceLog) seen() float64 {
	if l.open {
		return l.start
	}
	if n := len(l.closed); n > 0 {
		return l.closed[n-1].End
	}
	return 0
}

func (l *silenceLog) finish(end float64) []metadata.TimeRange {
	if l.open && end > l.start {
		l.closed = append(l.closed, metadata.TimeRange{Start: l.start, End: end})
	}
	return l.closed
}

// silenceField reads the number after key in line.
func silenceField(line, key string) (float64, bool) {
	i := strings.Index(line, key)
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(line[i+len(key):])
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(f[0], ","), 64)
	return v, err == nil
}
