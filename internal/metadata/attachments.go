package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/model"
)

// AttachmentsDir is the folder the Bridge writes a book's generated files into
// (BRIDGE-PLAN.md Phase 1a): in a folder book's own folder, or beside a
// single-file book with the file's stem before each name. It is hidden, so the
// scan never takes it for a book, and only these known names are read from it.
const AttachmentsDir = ".audiosilo"

// The files the server reads from an AttachmentsDir. characters.json,
// recaps.json and description.json are audiosilo-meta's own sidecar formats
// (model.Characters, model.Recaps, model.Description); chapters.json is the
// chapters the Bridge found in the narration of a book with none of its own
// (ChapterList, Phase 1d); bridge.json says what made them; readalong.json is
// the book's read-along map (Phase 3c: only its head is read, ReadalongHead).
const (
	AttachmentCharacters  = "characters.json"
	AttachmentRecaps      = "recaps.json"
	AttachmentDescription = "description.json"
	AttachmentChapters    = "chapters.json"
	AttachmentBridge      = "bridge.json"
	AttachmentReadalong   = "readalong.json"
)

// AttachmentNames are the known attachment names, in the order a signature
// lists them. A signature lists only the names a book has, and adding
// description.json and chapters.json kept the others in their order, as did
// appending readalong.json last, so a book without them keeps the signature it
// had before the names were known.
var AttachmentNames = []string{AttachmentCharacters, AttachmentRecaps, AttachmentDescription, AttachmentChapters,
	AttachmentBridge, AttachmentReadalong}

// The bounds an attachment is checked against: the pipeline's real output is a
// few hundred KB at most, so these only stop a file that isn't the pipeline's
// from costing the server memory or the player a page of noise.
const (
	maxAttachmentFile    = 4 << 20 // bytes in one file
	maxAttachmentEntries = 2000    // characters, or recaps, in one file
	maxAttachmentText    = 20000   // bytes in any one string
	maxAttachmentList    = 100     // aliases of a character, sources of a file
	maxProvenanceText    = 200     // bytes in a bridge.json value
	// A description's text, in code points once trimmed: audiosilo-meta's
	// description.schema.json bounds (minLength, maxLength).
	minDescriptionText = 200
	maxDescriptionText = 1500
)

// ErrAttachmentInvalid marks an attachment that was there but can't be used (not
// JSON, not the format, out of bounds, too large, not a regular file): reading
// it again won't help until it changes. Any other error from the Read functions
// means it couldn't be read right now (gone since it was found, a flaky mount),
// so a later read may succeed.
var ErrAttachmentInvalid = errors.New("unusable attachment")

var (
	errAttachmentTooLarge   = fmt.Errorf("%w: too large", ErrAttachmentInvalid)
	errAttachmentNotRegular = fmt.Errorf("%w: not a regular file", ErrAttachmentInvalid)
)

// AttachmentProvenance is what bridge.json says made a book's attachments: the
// generator and its version, the job kind and id, when, from what source (audio
// or ebook) and for which community work ("" when unresolved), and when the
// description was written (DescriptionAt, its own time: a description-only job
// keeps the characters' run as the file's generated_at). Every value is cleaned
// and bounded; a value that fails is left out, never the file.
type AttachmentProvenance struct {
	Generator     string `json:"generator,omitempty"`
	Version       string `json:"version,omitempty"`
	Kind          string `json:"kind,omitempty"`
	GeneratedAt   string `json:"generated_at,omitempty"`
	Source        string `json:"source,omitempty"`
	WorkID        string `json:"work_id,omitempty"`
	JobID         int64  `json:"job_id,omitempty"`
	DescriptionAt string `json:"description_at,omitempty"`
}

// AttachmentFiles is what a book's attachments say once checked: nil for a file
// that isn't there, is empty, or can't be used.
type AttachmentFiles struct {
	Characters  *model.Characters
	Recaps      *model.Recaps
	Description *model.Description
	Chapters    *ChapterList
	Provenance  *AttachmentProvenance
	Readalong   *ReadalongHead
}

// Empty reports whether there is nothing to store.
func (f AttachmentFiles) Empty() bool {
	return f.Characters == nil && f.Recaps == nil && f.Description == nil && f.Chapters == nil &&
		f.Provenance == nil && f.Readalong == nil
}

// Generated reports whether f holds generated content: characters, recaps or a
// description (provenance alone is none, and so are chapters: they are the
// book's chapters, not content /meta serves; and so is a read-along map).
func (f AttachmentFiles) Generated() bool {
	return f.Characters != nil || f.Recaps != nil || f.Description != nil
}

// AttachmentParts names a book's generated parts: which ones a reader wants
// (catalog.BookAttachments reads only those) or which ones a book has
// (catalog.BookAttachmentParts).
type AttachmentParts struct {
	Characters  bool
	Recaps      bool
	Description bool
}

// AllAttachmentParts is every generated part.
var AllAttachmentParts = AttachmentParts{Characters: true, Recaps: true, Description: true}

// Any reports whether p names any part.
func (p AttachmentParts) Any() bool {
	return p.Characters || p.Recaps || p.Description
}

// AttachmentSignature is one attachment's part of a book's signature: its known
// name, size and modification time (ns). The Bridge rewrites these files without
// touching the audio, so a scan compares them, not the book's own signature.
func AttachmentSignature(name string, info fs.FileInfo) string {
	return fmt.Sprintf("%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())
}

// AttachmentSigNames are the known names a book's signature lists (its
// AttachmentSignature parts, joined by commas).
func AttachmentSigNames(sig string) []string {
	var out []string
	for part := range strings.SplitSeq(sig, ",") {
		if name, _, ok := strings.Cut(part, ":"); ok {
			out = append(out, name)
		}
	}
	return out
}

// ReadCharacters reads and checks a characters.json (ParseCharacters), a regular
// file only, never through a link. An error wrapping ErrAttachmentInvalid is
// about the file itself.
func ReadCharacters(path string) (*model.Characters, error) {
	data, err := readAttachment(path)
	if err != nil {
		return nil, err
	}
	return ParseCharacters(data)
}

// ReadRecaps is ReadCharacters for a recaps.json (ParseRecaps).
func ReadRecaps(path string) (*model.Recaps, error) {
	data, err := readAttachment(path)
	if err != nil {
		return nil, err
	}
	return ParseRecaps(data)
}

// ReadDescription is ReadCharacters for a description.json (ParseDescription).
func ReadDescription(path string) (*model.Description, error) {
	data, err := readAttachment(path)
	if err != nil {
		return nil, err
	}
	return ParseDescription(data)
}

// ReadChapterList is ReadCharacters for a chapters.json (ParseChapterList).
func ReadChapterList(path string) (*ChapterList, error) {
	data, err := readAttachment(path)
	if err != nil {
		return nil, err
	}
	return ParseChapterList(data)
}

// ReadProvenance is ReadCharacters for a bridge.json (ParseProvenance).
func ReadProvenance(path string) (*AttachmentProvenance, error) {
	data, err := readAttachment(path)
	if err != nil {
		return nil, err
	}
	return ParseProvenance(data)
}

func readAttachment(path string) ([]byte, error) {
	return ReadRegularFile(path, maxAttachmentFile, errAttachmentNotRegular, errAttachmentTooLarge)
}

// decodeAttachment decodes data (a leading byte order mark skipped) into v,
// unknown fields allowed: a newer pipeline may add some.
func decodeAttachment(data []byte, v any) error {
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), v); err != nil {
		return fmt.Errorf("%w: %w", ErrAttachmentInvalid, err)
	}
	return nil
}

// invalidAttachment is ErrAttachmentInvalid with what failed.
func invalidAttachment(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAttachmentInvalid, fmt.Sprintf(format, args...))
}

// ParseCharacters decodes and checks a characters.json: every character has an
// id and a name and a reveal chapter >= 0, at most maxAttachmentEntries of them,
// every string within maxAttachmentText. Any entry that fails fails the file
// (the pipeline validates its output, so a failure means it isn't the
// pipeline's). nil, nil for a file with no characters.
func ParseCharacters(data []byte) (*model.Characters, error) {
	var c model.Characters
	if err := decodeAttachment(data, &c); err != nil {
		return nil, err
	}
	if len(c.Characters) > maxAttachmentEntries {
		return nil, invalidAttachment("%d characters", len(c.Characters))
	}
	if err := checkSidecar(c.Work, c.License, c.Sources); err != nil {
		return nil, err
	}
	for i, ch := range c.Characters {
		if strings.TrimSpace(ch.ID) == "" || strings.TrimSpace(ch.Name) == "" {
			return nil, invalidAttachment("character %d has no id or name", i)
		}
		if ch.Reveal.Chapter < 0 {
			return nil, invalidAttachment("character %d is revealed at chapter %d", i, ch.Reveal.Chapter)
		}
		if len(ch.Aliases) > maxAttachmentList {
			return nil, invalidAttachment("character %d has %d aliases", i, len(ch.Aliases))
		}
		texts := append([]string{ch.ID, ch.Name, ch.Role, ch.Description}, ch.Aliases...)
		if ch.Xref != nil {
			texts = append(texts, ch.Xref.Wikidata, ch.Xref.Goodreads)
		}
		if err := checkTexts(texts...); err != nil {
			return nil, fmt.Errorf("character %d: %w", i, err)
		}
	}
	if len(c.Characters) == 0 {
		return nil, nil
	}
	return &c, nil
}

// ParseRecaps decodes and checks a recaps.json: every recap has text and a
// through chapter >= 0, at most maxAttachmentEntries of them, every string within
// maxAttachmentText. As ParseCharacters, one entry failing fails the file. nil,
// nil for a file with no recaps and a blank summary.
func ParseRecaps(data []byte) (*model.Recaps, error) {
	var r model.Recaps
	if err := decodeAttachment(data, &r); err != nil {
		return nil, err
	}
	if len(r.Recaps) > maxAttachmentEntries {
		return nil, invalidAttachment("%d recaps", len(r.Recaps))
	}
	if err := checkSidecar(r.Work, r.License, r.Sources); err != nil {
		return nil, err
	}
	if err := checkTexts(r.InShort, r.Ending); err != nil {
		return nil, err
	}
	for i, rc := range r.Recaps {
		if strings.TrimSpace(rc.Text) == "" {
			return nil, invalidAttachment("recap %d has no text", i)
		}
		if rc.Through.Chapter < 0 {
			return nil, invalidAttachment("recap %d is through chapter %d", i, rc.Through.Chapter)
		}
		if err := checkTexts(rc.Scope, rc.Text); err != nil {
			return nil, fmt.Errorf("recap %d: %w", i, err)
		}
	}
	if len(r.Recaps) == 0 && strings.TrimSpace(r.InShort) == "" && strings.TrimSpace(r.Ending) == "" {
		return nil, nil
	}
	return &r, nil
}

// ParseDescription decodes and checks a description.json: the work, license
// and sources as the other sidecars, and a text of minDescriptionText to
// maxDescriptionText code points once trimmed (stored trimmed) with no control
// character but a line break. The text is shown as is in the player's About, so
// anything else fails the file. nil, nil for a blank text.
func ParseDescription(data []byte) (*model.Description, error) {
	var d model.Description
	if err := decodeAttachment(data, &d); err != nil {
		return nil, err
	}
	if err := checkSidecar(d.Work, d.License, d.Sources); err != nil {
		return nil, err
	}
	d.Text = strings.TrimSpace(d.Text)
	if d.Text == "" {
		return nil, nil
	}
	if n := utf8.RuneCountInString(d.Text); n < minDescriptionText || n > maxDescriptionText {
		return nil, invalidAttachment("a description of %d characters", n)
	}
	if strings.ContainsFunc(d.Text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		return nil, invalidAttachment("a control character in the description")
	}
	return &d, nil
}

// bridgeJSON is the part of bridge.json the server reads (its inputs list is
// the Bridge's own business; of its description object, only when, read by
// descriptionAt).
type bridgeJSON struct {
	Generator   string          `json:"generator"`
	Version     string          `json:"version"`
	Kind        string          `json:"kind"`
	JobID       int64           `json:"job_id"`
	GeneratedAt string          `json:"generated_at"`
	Source      string          `json:"source"`
	WorkID      string          `json:"work_id"`
	Description json.RawMessage `json:"description"`
}

// ParseProvenance decodes a bridge.json. Only a file that isn't a JSON object of
// the format fails; each value is cleaned (cleanProvenance), a generated_at that
// isn't RFC3339 is left out (it is re-written as RFC3339 UTC), and a negative job
// id is 0. nil, nil when it says nothing.
func ParseProvenance(data []byte) (*AttachmentProvenance, error) {
	var b bridgeJSON
	if err := decodeAttachment(data, &b); err != nil {
		return nil, err
	}
	p := AttachmentProvenance{
		Generator: cleanProvenance(b.Generator),
		Version:   cleanProvenance(b.Version),
		Kind:      cleanProvenance(b.Kind),
		Source:    cleanProvenance(b.Source),
		WorkID:    cleanProvenance(b.WorkID),
		JobID:     max(b.JobID, 0),
	}
	p.GeneratedAt = cleanTime(b.GeneratedAt)
	p.DescriptionAt = descriptionAt(b.Description)
	if p == (AttachmentProvenance{}) {
		return nil, nil
	}
	return &p, nil
}

// descriptionAt is bridge.json's description.generated_at (cleanTime), "" when
// there is no description object or it isn't the shape the server reads: a
// value like any other, never a reason to refuse the file.
func descriptionAt(raw json.RawMessage) string {
	var d struct {
		GeneratedAt string `json:"generated_at"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return ""
	}
	return cleanTime(d.GeneratedAt)
}

// cleanTime is a bridge.json time as RFC3339 UTC, "" when it isn't RFC3339.
func cleanTime(v string) string {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// cleanProvenance is a bridge.json value trimmed, or "" when it is longer than
// maxProvenanceText or carries a control character: these are shown in the
// console and logged, and a value is no reason to refuse the file.
func cleanProvenance(v string) string {
	return cleanText(v, maxProvenanceText)
}

// cleanText is v trimmed, or "" when it is longer than limit bytes, isn't
// UTF-8 or carries a control character.
func cleanText(v string, limit int) string {
	v = strings.TrimSpace(v)
	if len(v) > limit || !utf8.ValidString(v) {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

// checkSidecar checks the fields every sidecar format carries (characters,
// recaps, description): the work id and license (bounded like a provenance
// value) and the sources list.
func checkSidecar(work, license string, sources []model.Source) error {
	if len(work) > maxProvenanceText || len(license) > maxProvenanceText {
		return invalidAttachment("work or license too long")
	}
	if len(sources) > maxAttachmentList {
		return invalidAttachment("%d sources", len(sources))
	}
	for _, s := range sources {
		if err := checkTexts(s.Type, s.Ref, s.ImportedAt); err != nil {
			return err
		}
	}
	return nil
}

// checkTexts fails when any of texts is longer than maxAttachmentText.
func checkTexts(texts ...string) error {
	for _, t := range texts {
		if len(t) > maxAttachmentText {
			return invalidAttachment("a text of %d bytes", len(t))
		}
	}
	return nil
}
