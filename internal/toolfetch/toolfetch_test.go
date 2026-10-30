package toolfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Linux and Windows fetch BtbN's newest build of the pinned release branch
// (n8.1, whose -version the in-process probe knows), never master.
func TestSpecForBtbNAssets(t *testing.T) {
	t.Parallel()
	const base = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
	for platform, want := range map[string]spec{
		"linux/amd64":   {combinedURL: base + "ffmpeg-n8.1-latest-linux64-lgpl-8.1.tar.xz", combinedKind: "tar.xz"},
		"linux/arm64":   {combinedURL: base + "ffmpeg-n8.1-latest-linuxarm64-lgpl-8.1.tar.xz", combinedKind: "tar.xz"},
		"windows/amd64": {combinedURL: base + "ffmpeg-n8.1-latest-win64-lgpl-8.1.zip", combinedKind: "zip"},
		"windows/arm64": {combinedURL: base + "ffmpeg-n8.1-latest-winarm64-lgpl-8.1.zip", combinedKind: "zip"},
	} {
		goos, goarch, _ := strings.Cut(platform, "/")
		if got, ok := specFor(goos, goarch); !ok || got != want {
			t.Errorf("specFor(%s) = %+v, want %+v", platform, got, want)
		}
		if got, _ := specFor(goos, goarch); strings.Contains(got.combinedURL, "master") {
			t.Errorf("specFor(%s) fetches master: %s", platform, got.combinedURL)
		}
	}
}

func TestSpecFor(t *testing.T) {
	t.Parallel()
	for _, p := range []struct{ os, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		if _, ok := specFor(p.os, p.arch); !ok {
			t.Errorf("specFor(%s/%s) ok=false, want a source", p.os, p.arch)
		}
	}
	if _, ok := specFor("plan9", "mips"); ok {
		t.Error("specFor(unsupported) ok=true, want false")
	}
}

func TestCached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if fm, fp := Cached(dir); fm != "" || fp != "" {
		t.Fatalf("empty dir: Cached=%q,%q want empty", fm, fp)
	}
	// Create both tools and confirm they're detected.
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(dir, binName(tool)), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fm, fp := Cached(dir)
	if fm == "" || fp == "" {
		t.Fatalf("Cached after writing tools = %q,%q want both set", fm, fp)
	}
}

// Ensure must short-circuit (no download) when both tools are already cached.
func TestEnsureUsesCache(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the cached stubs are shell scripts")
	}
	dir := t.TempDir()
	// Stubs that pass Ensure's `-version` self-check: one that fails it is
	// discarded and re-downloaded, so the test would fetch a real build.
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		writeScript(t, filepath.Join(dir, binName(tool)), "#!/bin/sh\nexit 0\n")
	}
	fm, fp := Ensure(context.Background(), dir, discard())
	if fm != filepath.Join(dir, binName("ffmpeg")) || fp != filepath.Join(dir, binName("ffprobe")) {
		t.Fatalf("Ensure with cache = %q,%q want the cached paths", fm, fp)
	}
}

// extractZip must pull only ffmpeg/ffprobe (by basename, from any subdir) and mark
// them executable, ignoring everything else - and never escape destDir.
func TestExtractZip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"ffmpeg-build/bin/" + binName("ffmpeg"):  "FFMPEG",
		"ffmpeg-build/bin/" + binName("ffprobe"): "FFPROBE",
		"ffmpeg-build/README.txt":                "ignore me",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractZip(&buf, dir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	for tool, want := range map[string]string{"ffmpeg": "FFMPEG", "ffprobe": "FFPROBE"} {
		p := filepath.Join(dir, binName(tool))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("expected %s extracted: %v", tool, err)
		}
		if string(got) != want {
			t.Errorf("%s body = %q, want %q", tool, got, want)
		}
		if info, _ := os.Stat(p); info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable (mode %v)", tool, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Error("extractZip wrote a non-tool file")
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers\nbuilt with gcc": "6.1.1-3ubuntu5",
		"ffprobe version n7.0 Copyright": "n7.0",
		"something else entirely":        "",
		"":                               "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeStubs writes ffmpeg and ffprobe stubs into dir whose -version names
// version.
func writeStubs(t *testing.T, dir, version string) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		writeScript(t, filepath.Join(dir, binName(tool)), "#!/bin/sh\necho '"+tool+" version "+version+" Copyright (c) 2007-2026'\n")
	}
}

// writeScript writes an executable script at path holding syscall.ForkLock,
// so no exec another parallel test forks meanwhile inherits the file still open
// for writing; this test's own exec of it would otherwise fail with ETXTBSY
// ("text file busy", golang/go#22315) on Linux.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A cached master build (an older server's download, which turns the
// in-process probe off) is replaced by the pinned branch's on Linux and
// Windows, once; a failed download keeps it; a release build or another
// platform's source is left as it is (server#269).
func TestEnsureReplacesCachedMaster(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the cached stubs are shell scripts")
	}
	const master = "N-121510-g6e6a2b6f2c-20251028"
	for _, tc := range []struct {
		name, platform, cached string
		dlErr                  error
		wantDownloads          int
		wantVersion            string
	}{
		{"master on linux", "linux/amd64", master, nil, 1, "n8.1.3-25-g611cec792a-20261029"},
		{"master on windows", "windows/arm64", master, nil, 1, "n8.1.3-25-g611cec792a-20261029"},
		{"master, download fails", "linux/amd64", master, errors.New("offline"), 1, master},
		{"release build", "linux/amd64", "n8.1.3-25-g611cec792a-20261029", nil, 0, "n8.1.3-25-g611cec792a-20261029"},
		{"distribution build", "linux/amd64", "7.1.1-3ubuntu1", nil, 0, "7.1.1-3ubuntu1"},
		{"macOS master", "darwin/arm64", master, nil, 0, master},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeStubs(t, dir, tc.cached)
			downloads := 0
			dl := func(_ context.Context, s spec, into string) error {
				downloads++
				if !strings.Contains(s.combinedURL, "ffmpeg-n8.1-latest-") {
					t.Errorf("downloaded %q", s.combinedURL)
				}
				if tc.dlErr != nil {
					return tc.dlErr
				}
				writeStubs(t, into, "n8.1.3-25-g611cec792a-20261029")
				return nil
			}
			goos, goarch, _ := strings.Cut(tc.platform, "/")
			fm, fp := ensure(context.Background(), dir, discard(), goos, goarch, dl)
			if fm == "" || fp == "" || downloads != tc.wantDownloads {
				t.Fatalf("ensure = %q, %q after %d downloads, want both after %d", fm, fp, downloads, tc.wantDownloads)
			}
			if v, err := Version(context.Background(), fp); err != nil || v != tc.wantVersion {
				t.Fatalf("ffprobe version %q %v, want %q", v, err, tc.wantVersion)
			}
		})
	}
}
