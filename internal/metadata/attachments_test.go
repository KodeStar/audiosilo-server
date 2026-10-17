package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metaschema "github.com/kodestar/audiosilo-meta"
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

// descriptionText is a description of n code points (an accented letter each,
// so bytes and code points differ).
func descriptionText(n int) string { return strings.Repeat("é", n) }

// TestParseDescription: a pipeline-shaped file is read (unknown fields allowed,
// a byte order mark skipped, the text trimmed); a text out of its bounds (in code
// points), with a control character other than a line break, or a file that
// isn't the format fails; a blank text is nothing.
func TestParseDescription(t *testing.T) {
	t.Parallel()
	text := "A quiet harbour town.\n" + descriptionText(minDescriptionText)
	d, err := ParseDescription([]byte("\xef\xbb\xbf" + fmt.Sprintf(`{"work":"the-way-of-kings","text":%q,"license":"CC-BY-SA-4.0",
		"sources":[{"type":"bridge"}],"new_field":1}`, "  "+text+"\n")))
	if err != nil || d == nil || d.Work != "the-way-of-kings" || d.Text != text || d.License != "CC-BY-SA-4.0" || len(d.Sources) != 1 {
		t.Fatalf("valid file = %+v, %v", d, err)
	}
	for name, n := range map[string]int{"shortest": minDescriptionText, "longest": maxDescriptionText} {
		if d, err := ParseDescription([]byte(fmt.Sprintf(`{"text":%q}`, descriptionText(n)))); err != nil || d == nil {
			t.Errorf("%s: %+v, %v", name, d, err)
		}
	}
	for _, body := range []string{`{"work":"w","text":"   "}`, `{"work":"w"}`} {
		if d, err := ParseDescription([]byte(body)); d != nil || err != nil {
			t.Errorf("blank %s = %+v, %v; want nil, nil", body, d, err)
		}
	}
	for name, body := range map[string]string{
		"not JSON":      `not json`,
		"not an object": `["text"]`,
		"wrong type":    `{"text":12}`,
		"too short":     fmt.Sprintf(`{"text":%q}`, descriptionText(minDescriptionText-1)),
		"too long":      fmt.Sprintf(`{"text":%q}`, descriptionText(maxDescriptionText+1)),
		"a tab":         fmt.Sprintf(`{"text":%q}`, descriptionText(minDescriptionText)+"\tx"),
		"a return":      fmt.Sprintf(`{"text":%q}`, descriptionText(minDescriptionText)+"\r\nx"),
		// JSON's own escape: %q would write Go's \a, which fails as JSON before
		// the control-character rule is reached.
		"a bell":       `{"text":"` + descriptionText(minDescriptionText) + `\u0007x"}`,
		"a C1 control": fmt.Sprintf(`{"text":%q}`, descriptionText(minDescriptionText)+"\u0085x"),
		"long work":    fmt.Sprintf(`{"work":%q,"text":%q}`, strings.Repeat("w", maxProvenanceText+1), descriptionText(minDescriptionText)),
		"many sources": fmt.Sprintf(`{"text":%q,"sources":[%s]}`, descriptionText(minDescriptionText), strings.TrimSuffix(strings.Repeat(`{"type":"x"},`, maxAttachmentList+1), ",")),
	} {
		if d, err := ParseDescription([]byte(body)); !errors.Is(err, ErrAttachmentInvalid) {
			t.Errorf("%s: %+v, %v; want ErrAttachmentInvalid", name, d, err)
		}
	}
}

// TestDescriptionBoundsMatchTheSchema: the text bounds are audiosilo-meta's
// (description.schema.json's minLength and maxLength), so a newer meta that
// moves them shows up here rather than as files the server refuses.
func TestDescriptionBoundsMatchTheSchema(t *testing.T) {
	t.Parallel()
	raw, err := metaschema.SchemaFS.ReadFile("schema/description.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Text struct {
				MinLength int `json:"minLength"`
				MaxLength int `json:"maxLength"`
			} `json:"text"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties.Text; got.MinLength != minDescriptionText || got.MaxLength != maxDescriptionText {
		t.Fatalf("schema bounds = %d-%d, the server's = %d-%d", got.MinLength, got.MaxLength, minDescriptionText, maxDescriptionText)
	}
}

// TestParseProvenance: each value is cleaned on its own (a bad one is left out,
// never the file); the time is normalized to UTC.
func TestParseProvenance(t *testing.T) {
	t.Parallel()
	p, err := ParseProvenance([]byte(`{"generator":"audiosilo-bridge","version":"0.2.0","kind":"sidecars.generate","job_id":12,
		"generated_at":"2026-10-15T12:00:00+02:00","source":"audio","work_id":"","inputs":[{"path":"A/B/01.mp3","size":1,"mod_time":2}],
		"description":{"job_id":14,"generated_at":"2026-10-16T09:30:00+01:00","from":"opening_notes","chapters":3,"checked_against":"none"}}`))
	want := AttachmentProvenance{Generator: "audiosilo-bridge", Version: "0.2.0", Kind: "sidecars.generate", JobID: 12,
		GeneratedAt: "2026-10-15T10:00:00Z", Source: "audio", DescriptionAt: "2026-10-16T08:30:00Z"}
	if err != nil || p == nil || *p != want {
		t.Fatalf("provenance = %+v, %v; want %+v", p, err, want)
	}
	p, err = ParseProvenance([]byte(`{"generator":"bad\u0007name","version":"` + strings.Repeat("v", maxProvenanceText+1) + `","job_id":-3,"generated_at":"yesterday","kind":"k",
		"description":{"generated_at":"today"}}`))
	if err != nil || p == nil || *p != (AttachmentProvenance{Kind: "k"}) {
		t.Errorf("cleaned = %+v, %v; want only the kind", p, err)
	}
	// A description object of another shape costs only its time, never the file.
	for _, desc := range []string{`"written"`, `{"generated_at":1729065600}`, `[]`, `null`} {
		p, err := ParseProvenance([]byte(`{"kind":"k","job_id":3,"description":` + desc + `}`))
		if err != nil || p == nil || *p != (AttachmentProvenance{Kind: "k", JobID: 3}) {
			t.Errorf("description %s = %+v, %v; want the rest kept", desc, p, err)
		}
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
	desc := filepath.Join(dir, "description.json")
	if err := os.WriteFile(desc, []byte(fmt.Sprintf(`{"text":%q}`, descriptionText(minDescriptionText))), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, err := ReadDescription(desc); err != nil || d == nil {
		t.Fatalf("description: %+v, %v", d, err)
	}
	descLink := filepath.Join(dir, "linked.description.json")
	if err := os.Symlink(desc, descLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDescription(descLink); !errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("a linked description: %v, want ErrAttachmentInvalid", err)
	}
	if _, err := ReadProvenance(filepath.Join(dir, "missing.json")); err == nil || errors.Is(err, ErrAttachmentInvalid) {
		t.Errorf("missing: %v, want a read error that isn't ErrAttachmentInvalid", err)
	}
}
