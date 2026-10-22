// Package opml reads and writes the subscription-list subset of OPML that
// podcast apps exchange (Bridge Phase 2c-3): a bounded, strict parser that
// validates the whole document before anything acts on it, and the server's
// deterministic export. It is pure: no I/O but the reader it is handed, no
// network (entities, includes and linked OPML are never fetched).
package opml

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The import's bounds, over the whole document (ignored head and extension
// elements count too).
const (
	MaxBytes          = 1 << 20 // the body, read to one byte past it
	MaxOutlines       = 1000    // outlines in the body's tree, categories included
	MaxOutlineNesting = 16      // outlines inside outlines
	MaxDepth          = 32      // element depth, the root included
	MaxElements       = 5000
	MaxAttributes     = 32   // per element
	MaxAttributeBytes = 8192 // one decoded attribute value
)

// ErrInvalid is a document that is not a well-formed, supported OPML
// subscription list (a 400 invalid_opml); ErrLimit one past a bound (a 413
// opml_limit). Neither says more: the document is never echoed.
var (
	ErrInvalid = errors.New("invalid OPML document")
	ErrLimit   = errors.New("OPML document over a limit")
)

// ReadBody reads at most MaxBytes from r, ErrLimit when there is more,
// whatever a Content-Length said.
func ReadBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, ErrLimit
	}
	return data, nil
}

// Candidate is one outline that asks for a subscription: it has an xmlUrl
// attribute (any letter case), or it is type rss without one.
type Candidate struct {
	// Index is the outline's 1-based position among ALL the body's outlines in
	// document order, categories included.
	Index int
	// URL is the xmlUrl as written (entities decoded); "" when missing or blank.
	URL string
}

// Document is a parsed subscription list.
type Document struct {
	Version    string
	Outlines   int
	Candidates []Candidate
}

// CleanText is a title or name as plain text: control characters removed,
// spaces trimmed, at most max runes (invalid UTF-8 already replaced by the
// decoder that produced s, or here).
func CleanText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxRunes]))
	}
	return s
}

var bom = []byte{0xEF, 0xBB, 0xBF}

// trimBOM drops a leading UTF-8 byte order mark, which some apps write.
func trimBOM(data []byte) []byte { return bytes.TrimPrefix(data, bom) }
