package toolfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSpecFor(t *testing.T) {
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
	dir := t.TempDir()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(dir, binName(tool)), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fm, fp := Ensure(context.Background(), dir, discard())
	if fm != filepath.Join(dir, binName("ffmpeg")) || fp != filepath.Join(dir, binName("ffprobe")) {
		t.Fatalf("Ensure with cache = %q,%q want the cached paths", fm, fp)
	}
}

// extractZip must pull only ffmpeg/ffprobe (by basename, from any subdir) and mark
// them executable, ignoring everything else - and never escape destDir.
func TestExtractZip(t *testing.T) {
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
