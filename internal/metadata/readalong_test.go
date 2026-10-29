package metadata

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// readalongMap is a valid map as the Bridge writes it, with mutate applied to
// its decoded form first (nil for none).
func readalongMap(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"format": ReadalongFormat,
		"ebook":  map[string]any{"file": "Loyalists.epub", "sha256": testSHA},
		"audio": map[string]any{"duration": 84702.74,
			"files": []any{map[string]any{"path": "Loyalists.m4b", "duration": 84702.737}}},
		"granularity":  "sentence",
		"generator":    map[string]any{"name": "audiosilo-bridge", "version": "0.5.0", "aligner": "whisper-words/1", "asr": "whisper-large-v3-turbo"},
		"generated_at": "2026-11-02T10:11:12Z",
		"coverage":     map[string]any{"text": 0.996, "audio": 0.987},
		"sections": []any{map[string]any{"href": "Text/ch01.xhtml",
			"spans": []any{map[string]any{"cfi": "epubcfi(/6/4[ch01]!/4/2,/1:0,/1:118)", "start": 31.2, "end": 37.85}}}},
		"audio_gaps": []any{map[string]any{"start": 0, "end": 21.4}},
	}
	if mutate != nil {
		mutate(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestParseReadalongHead: a valid map's head is read whole (never its
// sections), its sha256 lowercased and its values cleaned.
func TestParseReadalongHead(t *testing.T) {
	t.Parallel()
	h, err := ParseReadalongHead(readalongMap(t, func(m map[string]any) {
		m["ebook"] = map[string]any{"file": "Loyalists.epub", "sha256": strings.ToUpper(testSHA)}
		m["generated_at"] = "2026-11-02T11:11:12+01:00"
		m["generator"] = map[string]any{"name": "audiosilo-bridge", "version": "bad\nvalue"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := ReadalongHead{
		Format:      ReadalongFormat,
		Ebook:       ReadalongEbook{File: "Loyalists.epub", SHA256: testSHA},
		Audio:       ReadalongAudio{Duration: 84702.74, Files: []ReadalongFile{{Path: "Loyalists.m4b", Duration: 84702.737}}},
		Generator:   &ReadalongGenerator{Name: "audiosilo-bridge"},
		GeneratedAt: "2026-11-02T10:11:12Z",
		Coverage:    ReadalongCoverage{Text: 0.996, Audio: 0.987},
	}
	got, _ := json.Marshal(h)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("head = %s\nwant   %s", got, exp)
	}
	// A generator that isn't an object, or says nothing, is left out.
	h, err = ParseReadalongHead(readalongMap(t, func(m map[string]any) { m["generator"] = "x"; delete(m, "generated_at") }))
	if err != nil || h.Generator != nil || h.GeneratedAt != "" {
		t.Errorf("no generator: %+v, %v", h, err)
	}
}

// TestParseReadalongHeadBounds: each check of the head fails the file.
func TestParseReadalongHeadBounds(t *testing.T) {
	t.Parallel()
	files := func(n int, path string, d any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"path": path, "duration": d}
		}
		return out
	}
	cases := map[string]func(m map[string]any){
		"another format":       func(m map[string]any) { m["format"] = "audiosilo-readalong/v2" },
		"no format":            func(m map[string]any) { delete(m, "format") },
		"no ebook":             func(m map[string]any) { delete(m, "ebook") },
		"a short sha256":       func(m map[string]any) { m["ebook"] = map[string]any{"sha256": testSHA[:63]} },
		"a sha256 not hex":     func(m map[string]any) { m["ebook"] = map[string]any{"sha256": strings.Repeat("g", 64)} },
		"no audio":             func(m map[string]any) { delete(m, "audio") },
		"no files":             func(m map[string]any) { m["audio"] = map[string]any{"files": []any{}} },
		"too many files":       func(m map[string]any) { m["audio"] = map[string]any{"files": files(5001, "a.mp3", 1)} },
		"an empty path":        func(m map[string]any) { m["audio"] = map[string]any{"files": files(1, "", 1)} },
		"a long path":          func(m map[string]any) { m["audio"] = map[string]any{"files": files(1, strings.Repeat("a", 1025), 1)} },
		"a zero duration":      func(m map[string]any) { m["audio"] = map[string]any{"files": files(1, "a.mp3", 0)} },
		"a negative duration":  func(m map[string]any) { m["audio"] = map[string]any{"files": files(1, "a.mp3", -1)} },
		"no duration":          func(m map[string]any) { m["audio"] = map[string]any{"files": []any{map[string]any{"path": "a.mp3"}}} },
		"no coverage":          func(m map[string]any) { delete(m, "coverage") },
		"text coverage over 1": func(m map[string]any) { m["coverage"] = map[string]any{"text": 1.01, "audio": 0.5} },
		"audio coverage < 0":   func(m map[string]any) { m["coverage"] = map[string]any{"text": 0.5, "audio": -0.1} },
		"a coverage missing":   func(m map[string]any) { m["coverage"] = map[string]any{"text": 0.5} },
		"the old number form":  func(m map[string]any) { m["coverage"] = 0.9 },
		"no sections":          func(m map[string]any) { delete(m, "sections") },
		"sections an object":   func(m map[string]any) { m["sections"] = map[string]any{} },
	}
	for name, mutate := range cases {
		if _, err := ParseReadalongHead(readalongMap(t, mutate)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for name, raw := range map[string]string{"not JSON": "{", "an array": "[]"} {
		if _, err := ParseReadalongHead([]byte(raw)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// At the bounds: 5000 files, a 1024-byte path, coverage 0 and 1.
	if _, err := ParseReadalongHead(readalongMap(t, func(m map[string]any) {
		m["audio"] = map[string]any{"files": files(5000, strings.Repeat("a", 1024), 0.001)}
		m["coverage"] = map[string]any{"text": 0, "audio": 1}
	})); err != nil {
		t.Errorf("at the bounds: %v", err)
	}
}

// TestReadReadalong: the map's own size bound (larger than the other
// attachments'), a regular file only.
func TestReadReadalong(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ok := filepath.Join(dir, "readalong.json")
	big := readalongMap(t, func(m map[string]any) {
		m["pad"] = strings.Repeat("x", maxAttachmentFile) // over the other files' bound, under the map's
	})
	if err := os.WriteFile(ok, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if h, err := ReadReadalong(ok); err != nil || h.Ebook.SHA256 != testSHA {
		t.Fatalf("a 4 MiB map = %+v, %v", h, err)
	}
	huge := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(huge, append(readalongMap(t, nil), make([]byte, MaxReadalongFile)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReadalong(huge); err == nil || !isInvalid(err) {
		t.Errorf("over 16 MiB: %v, want an invalid attachment", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReadalong(link); err == nil || !isInvalid(err) {
		t.Errorf("a link: %v, want an invalid attachment", err)
	}
	if _, err := ReadReadalong(filepath.Join(dir, "none.json")); err == nil || isInvalid(err) {
		t.Errorf("none: %v, want a read error that isn't the file's", err)
	}
}

// isInvalid reports whether err is about the file itself.
func isInvalid(err error) bool { return errors.Is(err, ErrAttachmentInvalid) }
