package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Silence is a pause in an audio file, in seconds from the file's start.
type Silence struct{ Start, End float64 }

// The pause a chapter break makes: at least silenceMinDur seconds under
// silenceNoise. silenceTimeout bounds one window's decode.
const (
	silenceNoise   = "-40dB"
	silenceMinDur  = "0.4"
	silenceTimeout = 30 * time.Second
)

// DetectSilences finds the pauses in [from, to] (seconds) of the audio file at
// absPath under the library root root with ffmpeg's silencedetect, seeking to
// from first, so a window costs a few seconds of decoding however long the
// file. A pause running past either edge of the window is cut at it. The file
// is opened through root (metadata.OpenRegularBelow) and handed to ffmpeg
// opened (metadata.ToolInput, within the same time), so ffmpeg never opens the path: a link swapped
// in along it since its check can't lead it out of the library. ffmpeg reads
// it as an audio container only (metadata.InputFormatArgs): a file shaped like
// a playlist is an error, the files it lists never opened.
func DetectSilences(ctx context.Context, ffmpegPath, root, absPath string, from, to float64) ([]Silence, error) {
	if to <= from {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, silenceTimeout)
	defer cancel()
	f, _, err := metadata.OpenRegularBelowBounded(ctx, root, absPath, silenceTimeout)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	input, extra, cleanup, err := metadata.ToolInput(ctx, f, root, absPath, silenceTimeout)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args := append([]string{"-nostdin", "-hide_banner", "-nostats",
		"-ss", strconv.FormatFloat(from, 'f', 3, 64),
		"-t", strconv.FormatFloat(to-from, 'f', 3, 64),
	}, metadata.InputFormatArgs()...)
	args = append(args, "-i", input,
		"-vn", "-af", "silencedetect=noise="+silenceNoise+":d="+silenceMinDur,
		"-f", "null", "-")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.ExtraFiles = extra
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("ffmpeg silencedetect: " + lastLine(stderr.String()))
	}
	return parseSilences(stderr.String(), from, to), nil
}

// parseSilences reads silencedetect's log (times from the window's start) into
// pauses on the file's timeline.
func parseSilences(log string, from, to float64) []Silence {
	var out []Silence
	start, open := 0.0, false // a pause under way when the window began
	sc := bufio.NewScanner(strings.NewReader(log))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := silenceField(line, "silence_start: "); ok {
			start, open = v, true
			continue
		}
		if v, ok := silenceField(line, "silence_end: "); ok {
			if !open {
				start = 0
			}
			out = append(out, Silence{Start: from + max(start, 0), End: from + v})
			open = false
		}
	}
	if open {
		out = append(out, Silence{Start: from + start, End: to})
	}
	return out
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
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
