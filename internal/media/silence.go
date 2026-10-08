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
// absPath with ffmpeg's silencedetect, seeking to from first, so a window costs a
// few seconds of decoding however long the file. A pause running past either edge
// of the window is cut at it.
func DetectSilences(ctx context.Context, ffmpegPath, absPath string, from, to float64) ([]Silence, error) {
	if to <= from {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, silenceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath, "-nostdin", "-hide_banner", "-nostats",
		"-ss", strconv.FormatFloat(from, 'f', 3, 64),
		"-t", strconv.FormatFloat(to-from, 'f', 3, 64),
		"-i", absPath,
		"-vn", "-af", "silencedetect=noise="+silenceNoise+":d="+silenceMinDur,
		"-f", "null", "-")
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
