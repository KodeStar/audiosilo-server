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

// specFor returns the download spec for an OS/arch, or ok=false when there's no
// known static source (the caller then just runs without ffmpeg).
func specFor(goos, goarch string) (spec, bool) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-linux64-lgpl.tar.xz", combinedKind: "tar.xz"}, true
	case "linux/arm64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-linuxarm64-lgpl.tar.xz", combinedKind: "tar.xz"}, true
	case "windows/amd64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-win64-lgpl.zip", combinedKind: "zip"}, true
	case "windows/arm64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-winarm64-lgpl.zip", combinedKind: "zip"}, true
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
// integrity check) — the caller degrades gracefully and retries next start.
func Ensure(ctx context.Context, dir string, log *slog.Logger) (ffmpeg, ffprobe string) {
	if ffmpeg, ffprobe = Cached(dir); ffmpeg != "" && ffprobe != "" {
		return ffmpeg, ffprobe
	}
	s, ok := specFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		log.Warn("no ffmpeg auto-download source for this platform; install ffmpeg to enable chapters/transcoding",
			"platform", runtime.GOOS+"/"+runtime.GOARCH)
		return Cached(dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Warn("ffmpeg auto-download: cannot create tools dir", "dir", dir, "err", err)
		return Cached(dir)
	}
	log.Info("ffmpeg/ffprobe not found locally — downloading a static build (one time)", "into", dir)
	if err := download(ctx, s, dir); err != nil {
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
// destDir (basename only — avoids zip-slip and skips the rest of the archive).
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

// writeExec writes r to path (0o755), replacing any existing file.
func writeExec(path string, r io.Reader) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
