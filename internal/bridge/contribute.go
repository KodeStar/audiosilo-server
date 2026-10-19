package bridge

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
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
	if len(p.Dimensions) == 0 {
		p.Dimensions = all
	}
	p.Dimensions = slices.DeleteFunc(all, func(d string) bool { return !slices.Contains(p.Dimensions, d) })
	return json.Marshal(p)
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
// the params or a change, is refused): a work id; at most 64 characters for
// the recording's identifiers and the region; 1 to MaxCorrections changes, each
// of a CorrectionFields field (once each), its values trimmed, single-line and
// at most 500 characters, the corrected one set and different from the current
// one; evidence required, at most 2000 characters (line breaks kept). It returns
// them as stored, the values trimmed.
func correctParams(raw json.RawMessage) (json.RawMessage, error) {
	var p CorrectParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	p.WorkID = strings.TrimSpace(p.WorkID)
	if p.WorkID == "" {
		return nil, invalid("work_id is required")
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
			return nil, err
		}
	}
	if len(p.Changes) == 0 || len(p.Changes) > MaxCorrections {
		return nil, invalid("changes must list 1 to %d fields", MaxCorrections)
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		if !slices.Contains(CorrectionFields, c.Field) {
			return nil, invalid("field %q can't be corrected", c.Field)
		}
		if slices.ContainsFunc(p.Changes[:i], func(o Correction) bool { return o.Field == c.Field }) {
			return nil, invalid("field %q is listed twice", c.Field)
		}
		c.Current, c.Corrected = strings.TrimSpace(c.Current), strings.TrimSpace(c.Corrected)
		for _, v := range []string{c.Current, c.Corrected} {
			if err := checkProse(c.Field, v, maxCorrectionValue, false); err != nil {
				return nil, err
			}
		}
		if c.Corrected == "" || c.Corrected == c.Current {
			return nil, invalid("%s: the corrected value must be set and differ from the current one", c.Field)
		}
	}
	p.Evidence = strings.TrimSpace(strings.ReplaceAll(p.Evidence, "\r\n", "\n"))
	if p.Evidence == "" {
		return nil, invalid("evidence is required")
	}
	if err := checkProse("evidence", p.Evidence, maxEvidence, true); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// CheckCorrect checks meta.correct's params as Enqueue will (correctParams)
// and returns them decoded, for the api's checks against the book before it
// queues them (the edited fields, the stamped source).
func CheckCorrect(raw json.RawMessage) (CorrectParams, error) {
	var p CorrectParams
	if len(raw) > maxParamsBytes {
		return p, invalid("params too large")
	}
	stored, err := correctParams(raw)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(stored, &p)
	return p, err
}

// checkProse bounds an admin's text: valid UTF-8, at most max characters, no
// control characters (but line breaks and tabs when lines allows them).
func checkProse(name, v string, max int, lines bool) error {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max ||
		strings.ContainsFunc(v, func(r rune) bool { return isControl(r) && !(lines && (r == '\n' || r == '\t')) }) {
		return invalid("%s must be at most %d characters of text", name, max)
	}
	return nil
}
