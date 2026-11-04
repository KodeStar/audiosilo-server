package metadata

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhowden/tag"
)

// TestProbeHints: ffprobe's (and the in-process probe's) tags name the ASIN by
// asin, else audible_asin (a TXXX or an MP4 freeform item by its name), the
// ISBN and the language; a garbage-long value is no hint, and a pass that has
// none leaves what an earlier one read.
func TestProbeHints(t *testing.T) {
	t.Parallel()
	m := &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{
		"audible_asin": "B0AUDIBLE1", "isbn": " 9780441013593 ", "language": "eng",
	}})
	if m.TagASIN != "B0AUDIBLE1" || m.TagISBN != "9780441013593" || m.TagLanguage != "eng" {
		t.Fatalf("hints = %q %q %q", m.TagASIN, m.TagISBN, m.TagLanguage)
	}
	applyProbe(m, &probeResult{Tags: map[string]string{"asin": "B0ASIN0001", "audible_asin": "B0AUDIBLE1"}})
	if m.TagASIN != "B0ASIN0001" || m.TagISBN != "9780441013593" {
		t.Errorf("asin beats audible_asin, and the ISBN stays: %q %q", m.TagASIN, m.TagISBN)
	}
	m = &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{"asin": strings.Repeat("B", maxHintBytes+1)}})
	if m.TagASIN != "" {
		t.Errorf("a garbage-long ASIN tag: %q", m.TagASIN)
	}
}

// TestTagHints: the tag pass (dhowden, no ffprobe) reads the same hints from
// every format's raw items: Vorbis keys, an MP4 freeform item (its value led
// by the data box's locale bytes), an ID3 TXXX frame by its description and
// ID3's TLAN.
func TestTagHints(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw              map[string]interface{}
		asin, isbn, lang string
	}{
		"vorbis": {map[string]interface{}{"asin": "B0VORBIS01", "isbn": "9780000000001", "language": "English"},
			"B0VORBIS01", "9780000000001", "English"},
		"mp4 freeform": {map[string]interface{}{"ASIN": "\x00\x00\x00\x00B0FREEFORM", "LANGUAGE": "de"},
			"B0FREEFORM", "", "de"},
		"id3 txxx and tlan": {map[string]interface{}{
			"TXXX":   &tag.Comm{Description: "SERIES", Text: "Dune"},
			"TXXX_0": &tag.Comm{Description: "ASIN", Text: "B0ID3TXXX1"},
			"TXXX_1": &tag.Comm{Description: "ISBN", Text: "9780000000002"},
			"TLAN":   "fre",
		}, "B0ID3TXXX1", "9780000000002", "fre"},
		"id3 audible_asin": {map[string]interface{}{"TXXX": &tag.Comm{Description: "AUDIBLE_ASIN", Text: "B0AUDIBLE2"}},
			"B0AUDIBLE2", "", ""},
		"none": {map[string]interface{}{"title": "Dune"}, "", "", ""},
	} {
		m := &Metadata{}
		applyTags(m, fakeTags{raw: tc.raw})
		if m.TagASIN != tc.asin || m.TagISBN != tc.isbn || m.TagLanguage != tc.lang {
			t.Errorf("%s: hints = %q %q %q, want %q %q %q", name, m.TagASIN, m.TagISBN, m.TagLanguage, tc.asin, tc.isbn, tc.lang)
		}
	}
}

// TestExtractReadsMP4FreeformASIN: an iTunes "----" item named ASIN, as
// Audible's tools write it, is read without ffprobe.
func TestExtractReadsMP4FreeformASIN(t *testing.T) {
	t.Parallel()
	freeform := func(name, value string) []byte {
		return box("----",
			box("mean", []byte{0, 0, 0, 0}, []byte("com.apple.iTunes")),
			box("name", []byte{0, 0, 0, 0}, []byte(name)),
			narratorData(1, value))
	}
	p := filepath.Join(t.TempDir(), "asin.m4b")
	if err := os.WriteFile(p, ilstFile(freeform("ASIN", "B0M4BASIN1"), freeform("ISBN", "9780000000003")), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Extract(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.TagASIN != "B0M4BASIN1" || m.TagISBN != "9780000000003" {
		t.Errorf("hints = %q %q", m.TagASIN, m.TagISBN)
	}
}

// TestExtractReadsHintTags: real files ffmpeg writes (an MP3's TXXX ASIN and
// TLAN, a FLAC's and an Ogg's Vorbis comments, an M4B's freeform items) give
// their hints in the tag pass alone and with ffprobe (the in-process probe, or
// ffprobe itself for a release it doesn't know).
func TestExtractReadsHintTags(t *testing.T) {
	t.Parallel()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	probes := []string{""}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		probes = append(probes, p)
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		file string
		args []string
	}{
		{"hints.mp3", nil},
		{"hints.flac", nil},
		{"hints.ogg", []string{"-c:a", "libvorbis"}},
		{"hints.m4b", []string{"-c:a", "aac", "-movflags", "+use_metadata_tags"}},
	} {
		p := filepath.Join(dir, tc.file)
		args := []string{"-v", "error", "-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono", "-t", "0.5",
			"-metadata", "title=Dune", "-metadata", "ASIN=B0FFMPEG01", "-metadata", "ISBN=9780000000004",
			"-metadata", "language=eng"}
		args = append(append(args, tc.args...), p)
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Logf("%s: ffmpeg can't write it here (%v: %s)", tc.file, err, out)
			continue
		}
		for _, probe := range probes {
			m, err := Extract(p, probe)
			if err != nil {
				t.Fatal(err)
			}
			if probe == "" && tc.file == "hints.m4b" {
				// ffmpeg writes an M4B's own keys as QuickTime mdta items, which
				// only ffprobe's pass reads.
				continue
			}
			if m.TagASIN != "B0FFMPEG01" || m.TagISBN != "9780000000004" || m.TagLanguage != "eng" {
				t.Errorf("%s (ffprobe %q): hints = %q %q %q", tc.file, probe, m.TagASIN, m.TagISBN, m.TagLanguage)
			}
		}
	}
}
