package metadata

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"strings"
)

// A book's read-along map (Bridge Phase 3c): .audiosilo/readalong.json, where
// each sentence of the book's ebook is spoken in its audio, written by the
// Bridge's readalong.align job. The server never parses the sections (the
// player does, span by span): it checks and stores the map's head, which says
// which ebook and which audio the map was made from, so it can tell a fresh map
// from a stale one (catalog.ReadalongState), and streams the file itself
// (GET /libraries/{id}/readalong).

// ReadalongFormat is the one format the server reads: a map in any other is
// none (a later format the player may not know).
const ReadalongFormat = "audiosilo-readalong/v1"

// MaxReadalongFile bounds a read-along map, its own bound rather than
// maxAttachmentFile's: a map holds one span per sentence, about 1.2 MB for a
// 23.5 h book, so 4 MiB would cap a book at about 80 h. The Bridge refuses to
// write one above it.
const MaxReadalongFile = 16 << 20

// The head's bounds: a book's audio files (a folder of parts), one file's path,
// and any other text value.
const (
	maxReadalongFiles = 5000
	maxReadalongPath  = 1024
)

var (
	errReadalongTooLarge   = fmt.Errorf("%w: too large", ErrAttachmentInvalid)
	errReadalongNotRegular = fmt.Errorf("%w: not a regular file", ErrAttachmentInvalid)
)

// ReadalongHead is a read-along map's head as the server stores it
// (book_attachments.readalong): everything but the sections.
type ReadalongHead struct {
	Format      string              `json:"format"`
	Ebook       ReadalongEbook      `json:"ebook"`
	Audio       ReadalongAudio      `json:"audio"`
	Generator   *ReadalongGenerator `json:"generator,omitempty"`
	GeneratedAt string              `json:"generated_at,omitempty"`
	Coverage    ReadalongCoverage   `json:"coverage"`
}

// ReadalongEbook is the ebook a map was made from: its file name, for a
// person, and its sha256 (64 lowercase hex), which the server compares.
type ReadalongEbook struct {
	File   string `json:"file,omitempty"`
	SHA256 string `json:"sha256"`
}

// ReadalongAudio is the audio a map was made from: the whole book's length
// and each file, book-relative (a folder book's relative to its folder, a
// single-file book's its base name), with its length in seconds.
type ReadalongAudio struct {
	Duration float64         `json:"duration"`
	Files    []ReadalongFile `json:"files"`
}

// ReadalongFile is one audio file a map was made from (or one of a book's
// files, to compare with it): its book-relative path and length in seconds.
type ReadalongFile struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

// ReadalongGenerator is what made a map (cleaned like a bridge.json value).
type ReadalongGenerator struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Aligner string `json:"aligner,omitempty"`
	ASR     string `json:"asr,omitempty"`
}

// ReadalongCoverage is how much of the ebook's read body (text) and of the
// audio (audio) the map lines up, each from 0 to 1.
type ReadalongCoverage struct {
	Text  float64 `json:"text"`
	Audio float64 `json:"audio"`
}

// readalongJSON is a map as the server decodes it: the head with every value
// it checks present or absent (pointers), and the sections left undecoded.
type readalongJSON struct {
	Format string          `json:"format"`
	Ebook  *ReadalongEbook `json:"ebook"`
	Audio  *struct {
		Duration float64 `json:"duration"`
		Files    []struct {
			Path     string   `json:"path"`
			Duration *float64 `json:"duration"`
		} `json:"files"`
	} `json:"audio"`
	Generator   json.RawMessage `json:"generator"`
	GeneratedAt string          `json:"generated_at"`
	Coverage    *struct {
		Text  *float64 `json:"text"`
		Audio *float64 `json:"audio"`
	} `json:"coverage"`
	Sections jsonArray `json:"sections"`
}

// jsonArray records whether a value is a JSON array without keeping it (the
// sections are most of a map, up to MaxReadalongFile bytes, and never stored).
type jsonArray bool

func (a *jsonArray) UnmarshalJSON(b []byte) error {
	*a = len(b) > 0 && b[0] == '['
	return nil
}

// ReadReadalong reads and checks a read-along map's head (ParseReadalongHead),
// a regular file only, never through a link, at most MaxReadalongFile bytes.
// An error wrapping ErrAttachmentInvalid is about the file itself.
func ReadReadalong(path string) (*ReadalongHead, error) {
	data, _, err := ReadReadalongFile(path)
	if err != nil {
		return nil, err
	}
	return ParseReadalongHead(data)
}

// ReadReadalongFile reads a read-along map's bytes as ReadReadalong does, with
// the opened file's info (GET /readalong's validators), never checking them.
func ReadReadalongFile(path string) ([]byte, fs.FileInfo, error) {
	return readRegularFileInfo(path, MaxReadalongFile, errReadalongNotRegular, errReadalongTooLarge)
}

// ParseReadalongHead decodes a read-along map and checks its head: format
// exactly ReadalongFormat, ebook.sha256 64 hex digits (stored lowercase), 1 to
// 5000 audio files each with a non-empty path of at most 1024 bytes and a
// duration above 0, coverage.text and coverage.audio present and from 0 to 1,
// and sections an array. Anything else fails the file (it isn't the Bridge's,
// or is a format this server doesn't know): a map that fails is none. The
// generator values and generated_at are cleaned as bridge.json's are (a value
// that fails is left out, never the file). Unknown fields are allowed.
func ParseReadalongHead(data []byte) (*ReadalongHead, error) {
	var j readalongJSON
	if err := decodeAttachment(data, &j); err != nil {
		return nil, err
	}
	if j.Format != ReadalongFormat {
		return nil, invalidAttachment("read-along format %q", cleanProvenance(j.Format))
	}
	if j.Ebook == nil || !isSHA256(j.Ebook.SHA256) {
		return nil, invalidAttachment("a read-along map with no ebook sha256")
	}
	if j.Audio == nil || len(j.Audio.Files) == 0 || len(j.Audio.Files) > maxReadalongFiles {
		return nil, invalidAttachment("a read-along map with no audio files, or too many")
	}
	if j.Coverage == nil || !unitInterval(j.Coverage.Text) || !unitInterval(j.Coverage.Audio) {
		return nil, invalidAttachment("a read-along map's coverage")
	}
	if !j.Sections {
		return nil, invalidAttachment("a read-along map with no sections")
	}
	h := &ReadalongHead{
		Format:      ReadalongFormat,
		Ebook:       ReadalongEbook{File: cleanText(j.Ebook.File, maxReadalongPath), SHA256: strings.ToLower(j.Ebook.SHA256)},
		Audio:       ReadalongAudio{Duration: max(j.Audio.Duration, 0), Files: make([]ReadalongFile, 0, len(j.Audio.Files))},
		GeneratedAt: cleanTime(j.GeneratedAt),
		Coverage:    ReadalongCoverage{Text: *j.Coverage.Text, Audio: *j.Coverage.Audio},
	}
	for i, f := range j.Audio.Files {
		if f.Path == "" || len(f.Path) > maxReadalongPath || f.Duration == nil || !(*f.Duration > 0) || math.IsInf(*f.Duration, 0) {
			return nil, invalidAttachment("read-along audio file %d", i)
		}
		h.Audio.Files = append(h.Audio.Files, ReadalongFile{Path: f.Path, Duration: *f.Duration})
	}
	var g ReadalongGenerator
	if json.Unmarshal(j.Generator, &g) == nil {
		g = ReadalongGenerator{Name: cleanProvenance(g.Name), Version: cleanProvenance(g.Version),
			Aligner: cleanProvenance(g.Aligner), ASR: cleanProvenance(g.ASR)}
		if g != (ReadalongGenerator{}) {
			h.Generator = &g
		}
	}
	return h, nil
}

// isSHA256 reports whether s is a sha256 digest in hex (64 digits, either case).
func isSHA256(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}

// unitInterval reports whether v is present and from 0 to 1.
func unitInterval(v *float64) bool {
	return v != nil && *v >= 0 && *v <= 1
}
