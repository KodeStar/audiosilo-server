package metadata

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The in-process probe answers every fixture it reads exactly as the installed
// ffprobe does (its release's rules): an FFmpeg release that changes a rule
// shows here as drift. AUDIOSILO_PROBE_DIRS (a path list) adds folders of real
// books to compare. It skips without an ffprobe on PATH, or with one whose
// release the probe follows no rules for, unless AUDIOSILO_PROBE_LIVE is set
// (CI's probe-drift job), when either is a failure.
func TestProbeMatchesLiveFFprobe(t *testing.T) {
	t.Parallel()
	required := os.Getenv("AUDIOSILO_PROBE_LIVE") != ""
	skip := t.Skipf
	if required {
		skip = t.Fatalf
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		skip("no ffprobe on PATH")
		return
	}
	release := ProbeRelease(t.Context(), ffprobe)
	if release == "" {
		out, _ := exec.CommandContext(t.Context(), ffprobe, "-version").Output()
		first, _, _ := strings.Cut(string(out), "\n")
		skip("%s: the probe follows no rules for this release (%s)", ffprobe, first)
		return
	}
	var files []string
	for _, dir := range append([]string{probeFixtures}, filepath.SplitList(os.Getenv("AUDIOSILO_PROBE_DIRS"))...) {
		if dir == "" {
			continue
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && IsAudio(p) && !strings.HasPrefix(d.Name(), "._") {
				files = append(files, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	reasons := map[string]int{}
	next := make(chan string)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for path := range next {
				got, reason, err := ProbeInProcess(t.Context(), path, ffprobe)
				if err != nil {
					t.Errorf("%s: %v", path, err)
					continue
				}
				if reason != "" {
					mu.Lock()
					reasons[fileFormat(path)+": "+reason]++
					mu.Unlock()
					continue
				}
				want, err := ProbeFFprobe(t.Context(), path, ffprobe)
				if err != nil {
					t.Errorf("%s: the probe answered, ffprobe failed: %v", path, err)
					continue
				}
				for _, d := range want.Diff(got) {
					t.Errorf("%s: %s (ffprobe %s)", path, d, release)
				}
			}
		})
	}
	for _, f := range files {
		next <- f
	}
	close(next)
	wg.Wait()
	var lines []string
	for r, n := range reasons {
		lines = append(lines, fmt.Sprintf("%4d %s", n, r))
	}
	slices.Sort(lines)
	t.Logf("%d files against ffprobe %s; left to ffprobe:\n%s", len(files), release, strings.Join(lines, "\n"))
}
