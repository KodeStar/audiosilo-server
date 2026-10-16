package metadata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCharacters: a pipeline-shaped file is read (unknown fields allowed, a
// byte order mark skipped); one entry out of bounds or missing what it needs
// fails the file; an empty list is nothing.
func TestParseCharacters(t *testing.T) {
	t.Parallel()
	c, err := ParseCharacters([]byte("\xef\xbb\xbf" + `{"work":"the-way-of-kings","license":"CC-BY-SA-4.0","new_field":1,
		"characters":[{"id":"kaladin","name":"Kaladin","aliases":["Kal"],"role":"protagonist","reveal":{"chapter":1},"description":"A spearman."}],
		"sources":[{"type":"bridge"}]}`))
	if err != nil || c == nil || c.Work != "the-way-of-kings" || len(c.Characters) != 1 || c.Characters[0].Aliases[0] != "Kal" {
		t.Fatalf("valid file = %+v, %v", c, err)
	}
	if c, err := ParseCharacters([]byte(`{"work":"w","characters":[]}`)); c != nil || err != nil {
		t.Errorf("empty list = %+v, %v; want nil, nil", c, err)
	}
	long := strings.Repeat("x", maxAttachmentText+1)
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"id":"a","name":"A","reveal":{"chapter":0}},`, maxAttachmentEntries+1), ",") + "]"
	for name, body := range map[string]string{
		"not JSON":         `not json`,
		"wrong type":       `{"characters":[{"id":"a","name":"A","reveal":"one"}]}`,
		"no id":            `{"characters":[{"name":"A","reveal":{"chapter":1}}]}`,
		"blank name":       `{"characters":[{"id":"a","name":"  ","reveal":{"chapter":1}}]}`,
		"negative reveal":  `{"characters":[{"id":"a","name":"A","reveal":{"chapter":-1}}]}`,
		"long description": fmt.Sprintf(`{"characters":[{"id":"a","name":"A","reveal":{"chapter":1},"description":%q}]}`, long),
		"too many":         `{"characters":` + many + `}`,
	} {
		if c, err := ParseCharacters([]byte(body)); !errors.Is(err, ErrAttachmentInvalid) {
			t.Errorf("%s: %+v, %v; want ErrAttachmentInvalid", name, c, err)
		}
	}
}

// TestParseRecaps: recaps need text and a through chapter >= 0; a file with only
// a whole-book summary is kept; one with nothing is nothing.
func TestParseRecaps(t *testing.T) {
	t.Parallel()
	r, err := ParseRecaps([]byte(`{"work":"w","recaps":[{"through":{"chapter":0},"text":"Before."},{"through":{"chapter":3},"scope":"book","text":"Three."}],
		"in_short":"In short.","ending":"Ends."}`))
	if err != nil || r == nil || len(r.Recaps) != 2 || r.InShort != "In short." || r.Ending != "Ends." {
		t.Fatalf("valid file = %+v, %v", r, err)
	}
	if r, err := ParseRecaps([]byte(`{"recaps":[],"in_short":"Only this."}`)); err != nil || r == nil {
		t.Errorf("summary only = %+v, %v; want kept", r, err)
	}
	if r, err := ParseRecaps([]byte(`{"recaps":[],"in_short":" "}`)); r != nil || err != nil {
		t.Errorf("nothing = %+v, %v; want nil, nil", r, err)
	}
	for name, body := range map[string]string{
		"no text":          `{"recaps":[{"through":{"chapter":1},"text":""}]}`,
		"negative through": `{"recaps":[{"through":{"chapter":-2},"text":"T"}]}`,
		"long ending":      fmt.Sprintf(`{"recaps":[],"ending":%q}`, strings.Repeat("x", maxAttachmentText+1)),
	} {
		if r, err := ParseRecaps([]byte(body)); !errors.Is(err, ErrAttachmentInvalid) {
			t.Errorf("%s: %+v, %v; want ErrAttachmentInvalid", name, r, err)
		}
	}
}

// TestParseProvenance: each value is cleaned on its own (a bad one is left out,
// never the file); the time is normalized to UTC.
func TestParseProvenance(t *testing.T) {
	t.Parallel()
	p, err := ParseProvenance([]byte(`{"generator":"audiosilo-bridge","version":"0.2.0","kind":"sidecars.generate","job_id":12,
		"generated_at":"2026-10-15T12:00:00+02:00","source":"audio","work_id":"","inputs":[{"path":"A/B/01.mp3","size":1,"mod_time":2}]}`))
	want := AttachmentProvenance{Generator: "audiosilo-bridge", Version: "0.2.0", Kind: "sidecars.generate", JobID: 12,
		GeneratedAt: "2026-10-15T10:00:00Z", Source: "audio"}
	if err != nil || p == nil || *p != want {
		t.Fatalf("provenance = %+v, %v; want %+v", p, err, want)
	}
	p, err = ParseProvenance([]byte(`{"generator":"bad\u0007name","version":"` + strings.Repeat("v", maxProvenanceText+1) + `","job_id":-3,"generated_at":"yesterday","kind":"k"}`))
	if err != nil || p == nil || *p != (AttachmentProvenance{Kind: "k"}) {
		t.Errorf("cleaned = %+v, %v; want only the kind", p, err)
	}
	if p, err := ParseProvenance([]byte(`{}`)); p != nil || err != nil {
		t.Errorf("empty = %+v, %v", p, err)
	}
	if _, err := ParseProvenance([]byte(`[1]`)); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("not an object: %v", err)
	}
}

// TestReadAttachmentFiles: a regular file is read; a link (even to a valid file)
// and a file over the size cap are unusable; a missing one is a read error that
// may work later.
func TestReadAttachmentFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := filepath.Join(dir, "characters.json")
	if err := os.WriteFile(valid, []byte(`{"characters":[{"id":"a","name":"A","reveal":{"chapter":1}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := ReadCharacters(valid); err != nil || c == nil {
		t.Fatalf("regular file: %+v, %v", c, err)
	}
	link := filepath.Join(dir, "linked.characters.json")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCharacters(link); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("a link: %v, want ErrAttachmentInvalid", err)
	}
	big := filepath.Join(dir, "recaps.json")
	if err := os.WriteFile(big, []byte(`{"recaps":[],"in_short":"`+strings.Repeat("x", maxAttachmentFile)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecaps(big); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("too large: %v, want ErrAttachmentInvalid", err)
	}
	if _, err := ReadProvenance(filepath.Join(dir, "missing.json")); err == nil || errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("missing: %v, want a read error that isn't ErrAttachmentInvalid", err)
	}
}
