package metadata

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The fixtures are ffmpeg m4bs (0.3 s of silence) whose iTunes items were
// byte-swapped into \xa9nrt, the atom Audible and most audiobook tools write:
// nrt.m4b had only a composer (\xa9wrt -> \xa9nrt, so it now has a narrator and
// no composer), nrt-composer.m4b a composer "A Composer" and a comment (\xa9cmt
// -> \xa9nrt). Both say "The Narrator".
const fixtureNarrator = "The Narrator"

// TestExtractReadsMP4Narrator (#177): the \xa9nrt item is the book's narrator,
// read with or without ffprobe (neither dhowden nor ffprobe returns it), and it
// beats a composer tag in both passes.
func TestExtractReadsMP4Narrator(t *testing.T) {
	t.Parallel()
	probes := []string{""}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		probes = append(probes, p)
	} else {
		t.Log("ffprobe not found: only the tag pass is checked")
	}
	for _, name := range []string{"nrt.m4b", "nrt-composer.m4b"} {
		for _, probe := range probes {
			m, err := Extract(filepath.Join("testdata", name), probe)
			if err != nil {
				t.Fatal(err)
			}
			if m.Narrator != fixtureNarrator {
				t.Errorf("%s (ffprobe %q): Narrator = %q, want %q", name, probe, m.Narrator, fixtureNarrator)
			}
			if m.Title != "Narrated" || m.Author != "Some Author" {
				t.Errorf("%s (ffprobe %q): title/author = %q/%q, the other tags lost", name, probe, m.Title, m.Author)
			}
		}
	}
}

// TestComposerStillNarratesWithoutNarratorTag: a file with no narrator tag still
// takes its composer, in the tag pass and in ffprobe's.
func TestComposerStillNarratesWithoutNarratorTag(t *testing.T) {
	t.Parallel()
	m := &Metadata{}
	applyTags(m, fakeTags{composer: "Comp"})
	applyProbe(m, &probeResult{Tags: map[string]string{"composer": "Comp Probe"}})
	if m.Narrator != "Comp Probe" {
		t.Errorf("Narrator = %q, want the composer", m.Narrator)
	}

	// An explicit one from the tags is not replaced by ffprobe's composer, but is
	// by ffprobe's own narrator tag.
	m = &Metadata{}
	applyTags(m, fakeTags{composer: "Comp", raw: map[string]interface{}{"NARRATOR": " Custom Item "}})
	if m.Narrator != "Custom Item" {
		t.Fatalf("NARRATOR item: Narrator = %q, want it over the composer", m.Narrator)
	}
	applyProbe(m, &probeResult{Tags: map[string]string{"composer": "Comp"}})
	if m.Narrator != "Custom Item" {
		t.Errorf("after ffprobe's composer: Narrator = %q, want the narrator tag kept", m.Narrator)
	}
	applyProbe(m, &probeResult{Tags: map[string]string{"narrator": "Probe Narrator", "composer": "Comp"}})
	if m.Narrator != "Probe Narrator" {
		t.Errorf("after ffprobe's narrator: Narrator = %q", m.Narrator)
	}
}

// box is an MP4 box: a 32-bit size, its name, its body.
func box(name string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(b)))
	return append(append(out, name...), b...)
}

func narratorData(class uint32, value string) []byte {
	body := binary.BigEndian.AppendUint32(nil, class)
	body = append(body, 0, 0, 0, 0) // locale
	return box("data", append(body, value...))
}

func ilstFile(items ...[]byte) []byte {
	meta := box("meta", []byte{0, 0, 0, 0}, box("hdlr", make([]byte, 25)), box("ilst", items...))
	return append(box("ftyp", []byte("M4A ")), box("moov", box("udta", meta))...)
}

// TestReadMP4NarratorBounded: the walk finds the item in the iTunes and the
// QuickTime meta forms and past a 64-bit-sized box, and a malformed or hostile
// file (truncated, a box claiming more than its parent, a size below its header,
// a non-text value, too long a value) yields "" without an error or a panic.
func TestReadMP4NarratorBounded(t *testing.T) {
	t.Parallel()
	read := func(b []byte) string { return readMP4Narrator(bytes.NewReader(b)) }
	good := ilstFile(box("\xa9nam", narratorData(1, "Title")), box("\xa9nrt", narratorData(1, " Kate Reading ")))
	if got := read(good); got != "Kate Reading" {
		t.Fatalf("iTunes meta: %q", got)
	}
	// QuickTime meta: no version/flags, hdlr straight away.
	qt := append(box("ftyp", []byte("qt  ")), box("moov", box("meta", box("hdlr", make([]byte, 25)),
		box("ilst", box("\xa9nrt", narratorData(1, "QT Narrator")))))...)
	if got := read(qt); got != "QT Narrator" {
		t.Errorf("QuickTime meta: %q", got)
	}
	// A 64-bit-sized mdat before moov.
	large := binary.BigEndian.AppendUint32(nil, 1)
	large = append(large, "mdat"...)
	large = binary.BigEndian.AppendUint64(large, 16+4)
	large = append(large, "abcd"...)
	if got := read(append(large, good...)); got != "Kate Reading" {
		t.Errorf("past a 64-bit box: %q", got)
	}
	for name, b := range map[string][]byte{
		"empty":          nil,
		"not mp4":        []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not an mp4 at all"),
		"truncated":      good[:len(good)-6],
		"no narrator":    ilstFile(box("\xa9nam", narratorData(1, "Title"))),
		"binary value":   ilstFile(box("\xa9nrt", narratorData(0, "\x00\x01"))),
		"too long":       ilstFile(box("\xa9nrt", narratorData(1, string(bytes.Repeat([]byte("a"), maxNarratorData+1))))),
		"oversized moov": append(box("ftyp", []byte("M4A ")), append(binary.BigEndian.AppendUint32(nil, 1<<30), "moov"...)...),
		"size below header": append(box("ftyp", []byte("M4A ")),
			append(binary.BigEndian.AppendUint32(nil, 4), "moov"...)...),
		"garbage ilst": ilstFile([]byte("\xff\xff\xff\xff\xa9nrtgarbage")),
	} {
		if got := read(b); got != "" {
			t.Errorf("%s: %q, want none", name, got)
		}
	}
}

// TestFixturesHoldNarratorAtom keeps the fixtures what the tests take them for.
func TestFixturesHoldNarratorAtom(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"nrt.m4b", "nrt-composer.m4b"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("\xa9nrt")) {
			t.Errorf("%s holds no \\xa9nrt item", name)
		}
	}
}
