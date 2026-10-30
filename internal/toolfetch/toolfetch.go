// Package toolfetch fetches ffmpeg/ffprobe on demand when the host doesn't already
// have them, so native (non-Docker) builds stay a small download instead of
// bundling ~100 MB of media tools most machines already have.
//
// Resolution order is owned by the caller (pkg/launcher): an explicit path, then
// next to the executable, then $PATH. Only when none of those turn up a tool does
// the caller fall back here, which caches a static build under <data>/tools/ and
// reuses it forever. Everything degrades gracefully: offline or an unsupported
// platform just means no ffmpeg (path-derived metadata still works, transcoding is
// off) and a retry on the next start.
//
// Integrity: downloads are HTTPS-only from pinned, reputable hosts (GitHub release
// assets from BtbN's FFmpeg-Builds for Linux/Windows; evermeet.cx for macOS), and
// every downloaded binary is sanity-checked by running `-version` before it's
// adopted. Pinning per-asset sha256 is a sensible future hardening step.
package toolfetch

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxToolBytes caps a single extracted binary (defends against a decompression
// bomb and is comfortably above ffmpeg's real size).
const maxToolBytes = 300 << 20 // 300 MiB

// spec describes where this platform's tools come from. Either combinedURL (one
// archive holding bin/ffmpeg + bin/ffprobe, BtbN) or the separate per-tool zips
// (evermeet) are set.
type spec struct {
	combinedURL  string
	combinedKind string // "tar.xz" | "zip"
	ffmpegURL    string // separate per-tool zips (each holds the bare binary)
	ffprobeURL   string
}

const btbn = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest"

// btbnBranch is the FFmpeg release branch the Linux and Windows builds come
// from: BtbN's "latest" release carries an n<branch>-latest asset per platform,
// rebuilt at each point release, whose -version reads "n8.1...". Never master:
// its "N-..." version names no release, and the in-process probe follows a
// known release's rules only (metadata.rulesFor), so a master build turned it
// off on every native Linux and Windows install (server#269). Move it only to
// a branch metadata's knownRules has.
const btbnBranch = "8.1"

// btbnAsset is the URL of BtbN's newest btbnBranch build for platform
// ("linux64", "win64", ...), an archive of kind ext ("tar.xz" or "zip").
func btbnAsset(platform, ext string) string {
	return btbn + "/ffmpeg-n" + btbnBranch + "-latest-" + platform + "-lgpl-" + btbnBranch + "." + ext
}

// specFor returns the download spec for an OS/arch, or ok=false when there's no
// known static source (the caller then just runs without ffmpeg).
func specFor(goos, goarch string) (spec, bool) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return spec{combinedURL: btbnAsset("linux64", "tar.xz"), combinedKind: "tar.xz"}, true
	case "linux/arm64":
		return spec{combinedURL: btbnAsset("linuxarm64", "tar.xz"), combinedKind: "tar.xz"}, true
	case "windows/amd64":
		return spec{combinedURL: btbnAsset("win64", "zip"), combinedKind: "zip"}, true
	case "windows/arm64":
		return spec{combinedURL: btbnAsset("winarm64", "zip"), combinedKind: "zip"}, true
	case "darwin/amd64", "darwin/arm64":
		// evermeet ships x86_64 only; on Apple Silicon it runs under Rosetta 2,
		// which is fine to exec from an arm64 server.
		return spec{
			ffmpegURL:  "https://evermeet.cx/ffmpeg/getrelease/ffmpeg/zip",
			ffprobeURL: "https://evermeet.cx/ffmpeg/getrelease/ffprobe/zip",
		}, true
	}
	return spec{}, false
}

// binName is the on-disk tool filename for this OS (ffmpeg / ffmpeg.exe).
func binName(tool string) string {
	if runtime.GOOS == "windows" {
		return tool + ".exe"
	}
	return tool
}

// Cached returns the cached paths for ffmpeg and ffprobe in dir, each "" if absent
// or not runnable.
func Cached(dir string) (ffmpeg, ffprobe string) {
	return cachedOne(dir, "ffmpeg"), cachedOne(dir, "ffprobe")
}

func cachedOne(dir, tool string) string {
	p := filepath.Join(dir, binName(tool))
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p
	}
	return ""
}

// Ensure returns usable ffmpeg/ffprobe paths under dir, downloading a static build
// for the current platform if they aren't cached yet. Either result is "" when the
// tool couldn't be made available (offline, unsupported platform, or a failed
// integrity check) - the caller degrades gracefully and retries next start. A
// cached BtbN master build is kept for this run while the release build is
// fetched in the background for the next start (replaceMaster).
func Ensure(ctx context.Context, dir string, log *slog.Logger) (ffmpeg, ffprobe string) {
	return ensure(ctx, dir, log, runtime.GOOS, runtime.GOARCH, download, func(f func()) { go f() })
}

// ensure is Ensure for platform goos/goarch, downloading with dl; bg runs the
// background release fetch.
func ensure(ctx context.Context, dir string, log *slog.Logger, goos, goarch string, dl func(context.Context, spec, string) error, bg func(func())) (ffmpeg, ffprobe string) {
	promoteStaged(ctx, dir, log)
	// Verify the cached copies rather than trusting a bare stat - a download killed
	// mid-write (before writeExec became atomic, or a disk/FS fault) can leave a
	// non-runnable binary that a stat happily reports as present. verified discards a
	// failing one so we fall through and re-download it.
	if ffmpeg, ffprobe = Cached(dir); ffmpeg != "" && ffprobe != "" {
		ffmpeg = verified(ctx, ffmpeg, log)
		ffprobe = verified(ctx, ffprobe, log)
		if ffmpeg != "" && ffprobe != "" {
			replaceMaster(ctx, dir, log, goos, goarch, dl, bg, ffprobe)
			return ffmpeg, ffprobe
		}
	}
	s, ok := specFor(goos, goarch)
	if !ok {
		log.Warn("no ffmpeg auto-download source for this platform; install ffmpeg to enable chapters/transcoding",
			"platform", goos+"/"+goarch)
		return Cached(dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Warn("ffmpeg auto-download: cannot create tools dir", "dir", dir, "err", err)
		return Cached(dir)
	}
	log.Info("ffmpeg/ffprobe not found locally - downloading a static build (one time)", "into", dir)
	if err := dl(ctx, s, dir); err != nil {
		log.Warn("ffmpeg auto-download failed; running without it (will retry next start)", "err", err)
		return Cached(dir)
	}
	ffmpeg, ffprobe = Cached(dir)
	// Adopt only binaries that actually run on this machine.
	ffmpeg = verified(ctx, ffmpeg, log)
	ffprobe = verified(ctx, ffprobe, log)
	if ffmpeg != "" || ffprobe != "" {
		log.Info("ffmpeg/ffprobe downloaded", "ffmpeg", ffmpeg != "", "ffprobe", ffprobe != "")
	}
	return ffmpeg, ffprobe
}

const (
	// stagedDir (under the tools dir) holds a release build fetched in the
	// background, swapped in at the next start (promoteStaged).
	stagedDir = "next"
	// failedMarker records when the last background release fetch failed
	// (RFC 3339), so a host that can't download doesn't retry every start.
	failedMarker = ".release-fetch-failed"
	// releaseRetry is how long a failed background fetch waits before the next.
	releaseRetry = 24 * time.Hour
)

// replaceMaster starts the swap of a cached BtbN master build (an older
// server's download, whose "N-..." version the in-process probe doesn't know)
// for the pinned branch's. It never blocks start-up: the cached pair serves this
// run, the release build is fetched in the background into <dir>/next and
// swapped in at the next start. A failed fetch is retried no sooner than
// releaseRetry later. Any other cached pair is left as it is.
func replaceMaster(ctx context.Context, dir string, log *slog.Logger, goos, goarch string, dl func(context.Context, spec, string) error, bg func(func()), ffprobe string) {
	s, ok := specFor(goos, goarch)
	if !ok || s.combinedURL == "" {
		return
	}
	if v, err := Version(ctx, ffprobe); err != nil || !strings.HasPrefix(v, "N-") {
		return
	}
	marker := filepath.Join(dir, failedMarker)
	if b, err := os.ReadFile(marker); err == nil {
		if at, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); perr == nil && time.Since(at) < releaseRetry {
			log.Info("the cached ffmpeg/ffprobe is a master build; the "+btbnBranch+" release download failed recently, keeping it",
				"retry_after", at.Add(releaseRetry).Format(time.RFC3339))
			return
		}
	}
	log.Info("the cached ffmpeg/ffprobe is a master build; fetching the "+btbnBranch+" release build in the background for the next start", "into", dir)
	bg(func() { fetchRelease(ctx, dir, log, s, dl) })
}

// fetchRelease downloads the release build into <dir>/next and checks it. A
// failure (not a shutdown) writes the back-off marker; a success clears it.
func fetchRelease(ctx context.Context, dir string, log *slog.Logger, s spec, dl func(context.Context, spec, string) error) {
	next := filepath.Join(dir, stagedDir)
	marker := filepath.Join(dir, failedMarker)
	fail := func(err error) {
		_ = os.RemoveAll(next)
		if ctx.Err() != nil {
			return // shut down mid-fetch: try again next start, no back-off
		}
		log.Warn("ffmpeg release download failed; keeping the cached build (retrying in 24 h)", "err", err)
		_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	_ = os.RemoveAll(next)
	if err := os.MkdirAll(next, 0o750); err != nil {
		fail(err)
		return
	}
	if err := dl(ctx, s, next); err != nil {
		fail(err)
		return
	}
	if fm, fp := Cached(next); verified(ctx, fm, log) == "" || verified(ctx, fp, log) == "" {
		fail(fmt.Errorf("the downloaded release build failed its self-check"))
		return
	}
	_ = os.Remove(marker)
	log.Info("ffmpeg/ffprobe " + btbnBranch + " release build downloaded; it is used from the next start")
}

// promoteStaged moves a checked release build from <dir>/next into dir (the
// background fetch of an earlier run). An incomplete or failing one is removed.
func promoteStaged(ctx context.Context, dir string, log *slog.Logger) {
	next := filepath.Join(dir, stagedDir)
	if _, err := os.Stat(next); err != nil {
		return
	}
	defer func() { _ = os.RemoveAll(next) }()
	fm, fp := Cached(next)
	if verified(ctx, fm, log) == "" || verified(ctx, fp, log) == "" {
		return
	}
	for _, p := range []string{fm, fp} {
		if err := os.Rename(p, filepath.Join(dir, filepath.Base(p))); err != nil {
			log.Warn("could not swap in the ffmpeg release build; keeping the cached one", "err", err)
			return
		}
	}
	log.Info("swapped in the ffmpeg/ffprobe "+btbnBranch+" release build", "dir", dir)
}

// verified returns path if `<path> -version` runs, else "" (and removes the file).
func verified(ctx context.Context, path string, log *slog.Logger) string {
	if path == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(cctx, path, "-version").Run(); err != nil {
		log.Warn("downloaded tool failed its self-check; discarding", "path", path, "err", err)
		_ = os.Remove(path)
		return ""
	}
	return path
}

// download fetches and extracts ffmpeg + ffprobe into dir per the platform spec.
func download(ctx context.Context, s spec, dir string) error {
	tmp, err := os.MkdirTemp("", "audiosilo-ffmpeg-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if s.combinedURL != "" {
		if err := fetchArchive(ctx, s.combinedURL, s.combinedKind, tmp); err != nil {
			return err
		}
	} else {
		if err := fetchArchive(ctx, s.ffmpegURL, "zip", tmp); err != nil {
			return err
		}
		if err := fetchArchive(ctx, s.ffprobeURL, "zip", tmp); err != nil {
			return err
		}
	}

	// Pull the two binaries out of whatever directory layout the archive used.
	want := map[string]bool{binName("ffmpeg"): true, binName("ffprobe"): true}
	found := 0
	err = filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !want[d.Name()] {
			return nil
		}
		if cerr := copyExec(p, filepath.Join(dir, d.Name())); cerr != nil {
			return cerr
		}
		found++
		return nil
	})
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("archive contained no ffmpeg/ffprobe binaries")
	}
	return nil
}

// fetchArchive downloads url and extracts it into destDir. tar.xz is handled by
// shelling out to the system tar (present on Linux/macOS); zip uses the stdlib.
func fetchArchive(ctx context.Context, url, kind, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	switch kind {
	case "tar.xz":
		f, err := os.CreateTemp(destDir, "dl-*.tar.xz")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := io.Copy(f, resp.Body); err != nil {
			f.Close()
			return err
		}
		f.Close()
		// -J = xz; tar contains the archive's path safety.
		cmd := exec.CommandContext(ctx, "tar", "-xJf", f.Name(), "-C", destDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract: %v: %s", err, out)
		}
		return nil
	case "zip":
		return extractZip(resp.Body, destDir)
	default:
		return fmt.Errorf("unknown archive kind %q", kind)
	}
}

// extractZip writes only the ffmpeg/ffprobe binaries from a zip stream into
// destDir (basename only - avoids zip-slip and skips the rest of the archive).
func extractZip(r io.Reader, destDir string) error {
	buf, err := io.ReadAll(io.LimitReader(r, maxToolBytes+1))
	if err != nil {
		return err
	}
	if int64(len(buf)) > maxToolBytes {
		return fmt.Errorf("zip exceeds %d bytes", maxToolBytes)
	}
	zr, err := zip.NewReader(strings.NewReader(string(buf)), int64(len(buf)))
	if err != nil {
		return err
	}
	want := map[string]bool{binName("ffmpeg"): true, binName("ffprobe"): true}
	for _, zf := range zr.File {
		base := filepath.Base(zf.Name)
		if zf.FileInfo().IsDir() || !want[base] {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeExec(filepath.Join(destDir, base), io.LimitReader(rc, maxToolBytes))
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// copyExec copies src to dst as an executable.
func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeExec(dst, io.LimitReader(in, maxToolBytes))
}

// writeExec writes r to path (0o755) atomically: it streams into a sibling temp
// file and renames it into place only after a clean close, so a process killed
// mid-copy can never leave a truncated binary at the final path (which Ensure
// would then adopt forever, since a stat can't tell a partial file from a whole
// one).
func writeExec(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed into place
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Dir is where Ensure keeps downloaded tools for a data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "tools") }

// Version runs `<path> -version` and returns the version it names: the word
// after "version" on its first line ("6.1.1-3ubuntu5" from "ffmpeg version
// 6.1.1-3ubuntu5 Copyright ..."), or "" if the line doesn't say.
func Version(ctx context.Context, path string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "-version").Output()
	if err != nil {
		return "", err
	}
	return parseVersion(string(out)), nil
}

func parseVersion(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "version" {
			return f[i+1]
		}
	}
	return ""
}
