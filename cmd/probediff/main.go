// Command probediff is a developer tool for the in-process probe (server#261,
// internal/metadata/fastprobe.go): it runs ffprobe and the in-process probe
// on every audio file under the given folders and reports where their answers
// differ, which files the probe leaves to ffprobe and why, and with -time how
// long each takes. With -golden it writes the probe fixtures' golden file for
// the ffprobe's release instead (scripts/gen-probe-fixtures.sh runs it).
//
//	go run ./cmd/probediff ~/audiobooks                # diff; exit 1 on a mismatch
//	go run ./cmd/probediff -time ~/audiobooks          # also time both, one file at a time
//	go run ./cmd/probediff -golden internal/metadata/testdata/probe
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func main() {
	ffprobe := flag.String("ffprobe", "ffprobe", "the ffprobe to compare with")
	workers := flag.Int("j", 4, "files read at once (not with -time)")
	timing := flag.Bool("time", false, "time both probes, one file at a time")
	golden := flag.String("golden", "", "write this fixture folder's golden-<release>.json instead of diffing")
	flag.Parse()
	ctx := context.Background()
	if *golden != "" {
		if err := writeGolden(ctx, *golden, *ffprobe); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	files := audioFiles(flag.Args()...)
	results := make([]result, len(files))
	if *timing {
		*workers = 1
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for range max(*workers, 1) {
		wg.Go(func() {
			for i := range next {
				results[i] = compare(ctx, files[i], *ffprobe, *timing)
			}
		})
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	if report(results, *timing) {
		os.Exit(1)
	}
}

// audioFiles is every audio file under roots, sorted (macOS's "._" files
// left out).
func audioFiles(roots ...string) []string {
	var files []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && metadata.IsAudio(p) && !strings.HasPrefix(d.Name(), "._") {
				files = append(files, p)
			}
			return nil
		})
	}
	slices.Sort(files)
	return files
}

// result is one file's comparison.
type result struct {
	path     string
	fallback string   // why the probe left the file to ffprobe
	ffErr    string   // ffprobe's error
	diffs    []string // where the probe's answer differs from ffprobe's
	ffTime   time.Duration
	fastTime time.Duration
}

func compare(ctx context.Context, path, ffprobe string, timing bool) result {
	r := result{path: path}
	t0 := time.Now()
	ff, err := metadata.ProbeFFprobe(ctx, path, ffprobe)
	r.ffTime = time.Since(t0)
	if err != nil {
		r.ffErr = err.Error()
	}
	t1 := time.Now()
	fast, reason, err := metadata.ProbeInProcess(ctx, path, ffprobe)
	r.fastTime = time.Since(t1)
	switch {
	case err != nil:
		r.fallback = "open: " + err.Error()
	case reason != "":
		r.fallback = reason
	case ff == nil:
		r.diffs = []string{"the probe answered, ffprobe failed: " + r.ffErr}
	default:
		r.diffs = ff.Diff(fast)
	}
	if !timing {
		r.ffTime, r.fastTime = 0, 0
	}
	return r
}

// report prints the per-format summary, the fallback reasons and the
// mismatches; true when there is a mismatch.
func report(rs []result, timing bool) bool {
	type row struct {
		files, fallbacks, mismatches int
		ff, fast                     time.Duration
	}
	byFormat := map[string]*row{}
	reasons := map[string]int{}
	mismatch := false
	for _, r := range rs {
		f := strings.TrimPrefix(strings.ToLower(filepath.Ext(r.path)), ".")
		if byFormat[f] == nil {
			byFormat[f] = &row{}
		}
		w := byFormat[f]
		w.files++
		w.ff += r.ffTime
		w.fast += r.fastTime
		if r.fallback != "" {
			w.fallbacks++
			reasons[f+": "+r.fallback]++
		}
		if len(r.diffs) > 0 {
			w.mismatches++
			mismatch = true
		}
	}
	fmt.Printf("%-5s %6s %9s %10s", "fmt", "files", "fallbacks", "mismatches")
	if timing {
		fmt.Printf(" %12s %12s", "ffprobe/file", "fast/file")
	}
	fmt.Println()
	for _, f := range slices.Sorted(maps.Keys(byFormat)) {
		w := byFormat[f]
		fmt.Printf("%-5s %6d %9d %10d", f, w.files, w.fallbacks, w.mismatches)
		if timing {
			n := time.Duration(w.files)
			fmt.Printf(" %12s %12s", (w.ff / n).Round(time.Microsecond), (w.fast / n).Round(time.Microsecond))
		}
		fmt.Println()
	}
	if len(reasons) > 0 {
		fmt.Println("\nleft to ffprobe:")
		for _, k := range slices.Sorted(maps.Keys(reasons)) {
			fmt.Printf("  %4d %s\n", reasons[k], k)
		}
	}
	for _, r := range rs {
		if len(r.diffs) == 0 {
			continue
		}
		fmt.Printf("\nMISMATCH %s\n", r.path)
		for _, d := range r.diffs {
			fmt.Printf("  %s\n", d)
		}
	}
	return mismatch
}

var ffAddr = regexp.MustCompile(`@ 0x[0-9a-f]+`)

// goldenFile is a fixture folder's golden-<release>.json: that release's
// ffprobe's answer for each fixture, and the probe's fallback reason where it
// leaves one to ffprobe. internal/metadata's probe_golden_test.go reads it.
type goldenFile struct {
	FFprobe string                  `json:"ffprobe"` // the ffprobe that wrote it (its -version line)
	Files   map[string]goldenAnswer `json:"files"`
}

type goldenAnswer struct {
	FFprobe  *metadata.ProbeView `json:"ffprobe,omitempty"`
	Error    string              `json:"ffprobe_error,omitempty"`
	Fallback string              `json:"fallback,omitempty"`
}

// writeGolden writes dir/golden-<release>.json from the fixtures in dir. It
// fails when the probe answers a fixture differently from ffprobe, or follows
// no rules for its release: a golden file never records a mismatch.
func writeGolden(ctx context.Context, dir, ffprobe string) error {
	release := metadata.ProbeRelease(ctx, ffprobe)
	if release == "" {
		return fmt.Errorf("%s: the probe knows no rules for this ffprobe's release", ffprobe)
	}
	ver, err := exec.CommandContext(ctx, ffprobe, "-version").Output()
	if err != nil {
		return fmt.Errorf("%s -version: %w", ffprobe, err)
	}
	g := goldenFile{FFprobe: strings.SplitN(string(ver), " Copyright", 2)[0], Files: map[string]goldenAnswer{}}
	for _, path := range audioFiles(dir) {
		name, _ := filepath.Rel(dir, path)
		var a goldenAnswer
		ff, err := metadata.ProbeFFprobe(ctx, path, ffprobe)
		if err != nil {
			// ffprobe names its demuxer by address ("[mov,mp4 @ 0xc7ac5c000]"), new each run.
			a.Error = ffAddr.ReplaceAllString(err.Error(), "@ 0x")
		}
		a.FFprobe = ff
		fast, reason, err := metadata.ProbeInProcess(ctx, path, ffprobe)
		switch {
		case err != nil:
			return err
		case reason != "":
			a.Fallback = reason
		case ff == nil:
			return fmt.Errorf("%s: the probe answered, ffprobe failed: %s", name, a.Error)
		default:
			if d := ff.Diff(fast); len(d) > 0 {
				return fmt.Errorf("%s: the probe differs from ffprobe: %s", name, strings.Join(d, "; "))
			}
		}
		g.Files[filepath.ToSlash(name)] = a
	}
	if len(g.Files) == 0 {
		return fmt.Errorf("no audio files in %s", dir)
	}
	out, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	fallbacks := 0
	for _, a := range g.Files {
		if a.Fallback != "" {
			fallbacks++
		}
	}
	fmt.Printf("%d fixtures, %d left to ffprobe (%s)\n", len(g.Files), fallbacks, g.FFprobe)
	return os.WriteFile(filepath.Join(dir, "golden-"+release+".json"), append(out, '\n'), 0o644) //nolint:gosec // G306: a checked-in test fixture
}
