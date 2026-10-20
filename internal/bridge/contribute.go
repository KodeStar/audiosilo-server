package bridge

import (
	"encoding/json"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/model"
)

// Phase 2b's community jobs (BRIDGE-PLAN.md "Community contributions"): the
// Bridge files to the community metadata database with its GitHub sign-in, and
// the server only checks what an admin asked for. Both kinds write nothing into
// the library and need the contributions module (kindSpec.module).

// The dimensions a sidecars.contribute job may file, in the order they are
// stored.
const (
	DimensionCharacters = "characters"
	DimensionRecaps     = "recaps"
)

// ContributeParams are sidecars.contribute's params as stored: the community
// work the results are for when the caller knows it (else the Bridge's own
// match), and which of the book's generated files to file (both by default).
type ContributeParams struct {
	WorkID     string   `json:"work_id,omitempty"`
	Dimensions []string `json:"dimensions"`
}

// contributeParams checks sidecars.contribute's params strictly (an unknown
// field is refused): a work id as sidecars.generate's, and dimensions from
// characters and recaps, each at most once, stored in that order; none (absent
// or empty) is both.
func contributeParams(raw json.RawMessage) (json.RawMessage, error) {
	var p ContributeParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if err := checkText("work_id", p.WorkID, maxWorkIDLen); err != nil {
		return nil, err
	}
	all := []string{DimensionCharacters, DimensionRecaps}
	for i, d := range p.Dimensions {
		if !slices.Contains(all, d) || slices.Contains(p.Dimensions[:i], d) {
			return nil, invalid("dimensions are characters and recaps, each once")
		}
	}
	p.Dimensions = slices.DeleteFunc(all, func(d string) bool {
		return len(p.Dimensions) > 0 && !slices.Contains(p.Dimensions, d)
	})
	return json.Marshal(p)
}

// SharedNothing reports whether j is a sidecars.contribute job that finished
// having filed nothing: its result (the Bridge's) names no contribution
// because the community already had each dimension it was asked for (covered).
func (j *Job) SharedNothing() bool {
	if j.Kind != KindContribute || j.State != StateDone {
		return false
	}
	var r struct {
		Contributed map[string]json.RawMessage `json:"contributed"`
		Covered     []string                   `json:"covered"`
	}
	return json.Unmarshal(j.Result, &r) == nil && len(r.Contributed) == 0 && len(r.Covered) > 0
}

// CorrectionFields are the book fields a meta.correct job may correct (the
// catalog's field names, CORRECTIONS-SPIKE.md section 5): the Bridge maps each
// to the community's records. The api checks each against the book's own
// values (an admin's edit, catalog.SourceEdited).
var CorrectionFields = []string{
	"title", "author", "narrator", "series", "series_index", "more_series", "published", "isbn",
}

// Bounds on a meta.correct job.
const (
	MaxCorrections     = 25
	maxCorrectionValue = 500  // characters
	maxEvidence        = 2000 // characters
	maxCorrectIDLen    = 64   // asin, isbn, region
	maxSourceLen       = 128
	// maxCorrectParamsBytes bounds meta.correct's params as sent: a batch at
	// every bound above (each of the 8 fields once, two 500-character values,
	// 2000 characters of evidence) is 10,000 characters, at most 12 bytes each
	// in JSON (a character outside the BMP sent as two \u escapes, as an
	// ASCII-only encoder does), so 120,000 bytes; its identifiers (work_id,
	// recording_id, asin, isbn, region, source: 720 bytes at most, each byte
	// at most 6 escaped) and the names around them fit in the rest.
	maxCorrectParamsBytes = 128 << 10
)

// CorrectParams are meta.correct's params as stored: the community records
// the book is matched to (the work, and the recording or the identifiers that
// pick one), the changes (each field's community value now and the admin's
// value), the admin's evidence, and the server that sent it (Source, which the
// api stamps: audiosilo-server/<version>).
type CorrectParams struct {
	WorkID      string       `json:"work_id"`
	RecordingID string       `json:"recording_id,omitempty"`
	ASIN        string       `json:"asin,omitempty"`
	ISBN        string       `json:"isbn,omitempty"`
	Region      string       `json:"region,omitempty"`
	Changes     []Correction `json:"changes"`
	Evidence    string       `json:"evidence"`
	Source      string       `json:"source,omitempty"`
}

// Correction is one field's change: the community's value now ("" when it has
// none) and the corrected value (the book's edited one).
type Correction struct {
	Field     string `json:"field"`
	Current   string `json:"current"`
	Corrected string `json:"corrected"`
}

// correctParams checks meta.correct's params strictly (an unknown field, in
// the params or a change, is refused): a work id; a recording id, when given,
// a community slug (model.ValidSlug, as the Bridge requires); at most 64
// characters for the recording's other identifiers and the region; 1 to
// MaxCorrections changes, each of a CorrectionFields field (once each), its
// values trimmed, single-line and at most 500 characters, the corrected one set
// and different from the current one as the community reads it (asRead);
// evidence required (none when it reads as nothing), at most 2000 characters
// (line breaks kept) both as written and as the community's issue carries it
// (issueLength). Neither a value nor the evidence holds a control character
// (unicode.IsControl, the Bridge's test; the evidence keeps line breaks and
// tabs) or a bidirectional override (isBidiControl, the community's), as
// written or with its references decoded (checkProse). It returns them as
// stored, the values trimmed.
func correctParams(raw json.RawMessage) (json.RawMessage, error) {
	p, err := parseCorrect(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// parseCorrect is correctParams before it encodes them: the params checked,
// decoded and trimmed.
func parseCorrect(raw json.RawMessage) (CorrectParams, error) {
	var p CorrectParams
	if err := decodeStrict(raw, &p); err != nil {
		return p, invalid("params: %v", err)
	}
	p.WorkID = strings.TrimSpace(p.WorkID)
	if p.WorkID == "" {
		return p, invalid("work_id is required")
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{
		{"work_id", p.WorkID, maxWorkIDLen}, {"recording_id", p.RecordingID, maxWorkIDLen},
		{"asin", p.ASIN, maxCorrectIDLen}, {"isbn", p.ISBN, maxCorrectIDLen}, {"region", p.Region, maxCorrectIDLen},
		{"source", p.Source, maxSourceLen},
	} {
		if err := checkText(f.name, f.v, f.max); err != nil {
			return p, err
		}
	}
	p.RecordingID = strings.TrimSpace(p.RecordingID)
	if p.RecordingID != "" && !model.ValidSlug(p.RecordingID) {
		return p, invalid("recording_id must be a community recording id")
	}
	if len(p.Changes) == 0 || len(p.Changes) > MaxCorrections {
		return p, invalid("changes must list 1 to %d fields", MaxCorrections)
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		if !slices.Contains(CorrectionFields, c.Field) {
			return p, invalid("field %q can't be corrected", c.Field)
		}
		if slices.ContainsFunc(p.Changes[:i], func(o Correction) bool { return o.Field == c.Field }) {
			return p, invalid("field %q is listed twice", c.Field)
		}
		c.Current, c.Corrected = strings.TrimSpace(c.Current), strings.TrimSpace(c.Corrected)
		for _, v := range []string{c.Current, c.Corrected} {
			if err := checkProse(c.Field, v, maxCorrectionValue, false); err != nil {
				return p, err
			}
		}
		// The community applies the corrected value, as it reads it, to the
		// record, whose value is current as the record spells it: a corrected
		// "Tom &amp; Jerry" changes nothing in a "Tom & Jerry", and corrects a
		// "Tom &amp; Jerry".
		if corrected := asRead(c.Corrected); corrected == "" || corrected == c.Current {
			return p, invalid("%s: the corrected value must be set and differ from the current one", c.Field)
		}
	}
	p.Evidence = strings.TrimSpace(strings.ReplaceAll(p.Evidence, "\r\n", "\n"))
	if p.Evidence == noResponse || asRead(p.Evidence) == "" {
		return p, invalid("evidence is required")
	}
	if err := checkProse("evidence", p.Evidence, maxEvidence, true); err != nil {
		return p, err
	}
	if issueLength(p.Evidence) > maxEvidence {
		return p, invalid("evidence must be at most %d characters, each line starting with # counting one more", maxEvidence)
	}
	return p, nil
}

// issueLength is evidence's length in characters as the community's issue
// carries it, which the community and the Bridge bound: each line that starts
// with # (after its indent) escaped, the indent dropped and a backslash added
// (the Bridge's contrib.escapeHeadings).
func issueLength(evidence string) int {
	n := utf8.RuneCountInString(evidence)
	for line := range strings.SplitSeq(evidence, "\n") {
		if t := strings.TrimLeft(line, " \t"); strings.HasPrefix(t, "#") {
			n += 1 - (len(line) - len(t)) // the indent is spaces and tabs, a byte each
		}
	}
	return n
}

// CheckCorrect checks meta.correct's params as Enqueue will (the kind's cap,
// correctParams) and returns them decoded, for the api's checks against the
// book before it queues them (the edited fields, the stamped source). On the
// api's path it is the cap on the params as sent: Enqueue checks the api's own
// encoding of them.
func CheckCorrect(raw json.RawMessage) (CorrectParams, error) {
	if len(raw) > kinds[KindCorrect].paramsCap() {
		return CorrectParams{}, invalid("params too large")
	}
	return parseCorrect(raw)
}

// checkProse bounds an admin's text: valid UTF-8, at most max characters, and,
// its references decoded (decodeRefs, untrimmed: a trim could drop a C1 space
// like U+0085 before a trailing "&#32;" the Bridge still files), no control
// characters (unicode.IsControl, C1 included as the Bridge refuses them; but
// line breaks and tabs when lines allows them) and no bidirectional override
// (isBidiControl), which the community refuses in a batch.
func checkProse(name, v string, max int, lines bool) error {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		return invalid("%s must be at most %d characters of text", name, max)
	}
	decoded := decodeRefs(v)
	if strings.ContainsFunc(decoded, func(r rune) bool { return unicode.IsControl(r) && !(lines && (r == '\n' || r == '\t')) }) {
		return invalid("%s holds a control character", name)
	}
	if strings.ContainsFunc(decoded, isBidiControl) {
		return invalid("%s holds a bidirectional override character", name)
	}
	return nil
}

// The community's issue form reads a field as GitHub shows it (audiosilo-meta's
// issueform: parseBody, formValue): a field left blank arrives as noResponse,
// which it reads as empty, and an HTML character reference ending in ';' is
// the character it names, a whitespace one a space. It checks the values and
// the evidence read that way, so the checks here read them that way too: an
// "&#x202E;" is a bidirectional override (GitHub shows the evidence's as one).
const noResponse = "_No response_"

// charRef is an HTML character reference ending in ';': decimal, hexadecimal
// ("&#X" too, which GitHub decodes though the community doesn't) or named.
var charRef = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);`)

// asRead is v as the community reads it: its references decoded (decodeRefs),
// then trimmed.
func asRead(v string) string {
	return strings.TrimSpace(decodeRefs(v))
}

// decodeRefs is v with each character reference decoded as the community
// reads it: one decoding to whitespace read as a space, and one decoding to
// more than two characters left as written (it isn't one: html.UnescapeString
// reads "&copyright;" as "&copy" and "right;"). A reference is ASCII, so every
// other character of v stays as written.
func decodeRefs(v string) string {
	if !strings.Contains(v, "&") {
		return v
	}
	return charRef.ReplaceAllStringFunc(v, func(ref string) string {
		r := html.UnescapeString(ref)
		if utf8.RuneCountInString(r) > 2 {
			return ref
		}
		if c, n := utf8.DecodeRuneInString(r); n == len(r) && unicode.IsSpace(c) {
			return " "
		}
		return r
	})
}

// isBidiControl reports the bidirectional embedding, override and isolate
// controls (U+202A-U+202E, U+2066-U+2069), which reorder how text displays
// (audiosilo-meta's issueform refuses them). The joiners (U+200C, U+200D) and
// the marks (U+200E, U+200F) that right-to-left scripts need are not among them.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}
