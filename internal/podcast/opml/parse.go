package opml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// element kinds on the parser's stack.
const (
	kindOther = iota
	kindRoot
	kindHead
	kindBody
	kindOutline // an outline in the body's tree
)

type frame struct {
	kind    int
	nesting int // outlines up to and including this one
}

// Parse validates a whole subscription list and returns its candidates. It
// accepts one unnamespaced <opml version="1.0|2.0"> root holding at most one
// head and exactly one body, UTF-8 only, with nothing but an initial XML
// declaration, comments and whitespace outside it. Any DOCTYPE or other
// directive (an internal or external DTD, used or not), any other processing
// instruction, an unknown entity, a duplicate attribute (xmlUrl in any two
// letter cases counts), a second root or a malformed tail is ErrInvalid; a
// bound is ErrLimit. Nothing is resolved or fetched. Only outlines inside the
// body's outline tree are read; categories are flattened in document order.
func Parse(data []byte) (*Document, error) {
	if len(data) > MaxBytes {
		return nil, ErrLimit
	}
	data = trimBOM(data)
	if !utf8.Valid(data) {
		return nil, ErrInvalid
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	// Entity and CharsetReader stay nil: only XML's own five entities and
	// numeric references decode, and a declared non-UTF-8 encoding fails.
	doc := &Document{}
	var stack []frame
	first, rootDone, heads, bodies, elements := true, false, 0, 0, 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrInvalid
		}
		isFirst := first
		first = false
		switch t := tok.(type) {
		case xml.ProcInst:
			// The decoder itself refuses a version other than 1.0 and an
			// encoding other than UTF-8 in the declaration.
			if !isFirst || t.Target != "xml" {
				return nil, ErrInvalid
			}
		case xml.Directive:
			return nil, ErrInvalid
		case xml.Comment:
		case xml.CharData:
			// Outside the root only XML's own whitespace (no NBSP or other
			// Unicode space, which bytes.TrimSpace would pass).
			if len(stack) == 0 && len(bytes.Trim(t, " \t\r\n")) > 0 {
				return nil, ErrInvalid
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			rootDone = len(stack) == 0
		case xml.StartElement:
			if rootDone {
				return nil, ErrInvalid // a second root
			}
			elements++
			if elements > MaxElements || len(stack)+1 > MaxDepth || len(t.Attr) > MaxAttributes {
				return nil, ErrLimit
			}
			if err := checkAttrs(t.Attr); err != nil {
				return nil, err
			}
			f, err := doc.open(t, stack, &heads, &bodies)
			if err != nil {
				return nil, err
			}
			stack = append(stack, f)
		}
	}
	if !rootDone || bodies != 1 {
		return nil, ErrInvalid
	}
	return doc, nil
}

// checkAttrs refuses an element whose attributes repeat a name, or a value
// over MaxAttributeBytes.
func checkAttrs(attrs []xml.Attr) error {
	for i, a := range attrs {
		if len(a.Value) > MaxAttributeBytes {
			return ErrLimit
		}
		for _, b := range attrs[:i] {
			if a.Name == b.Name || a.Name.Space == "" && b.Name.Space == "" && strings.EqualFold(a.Name.Local, "xmlurl") && strings.EqualFold(b.Name.Local, "xmlurl") {
				return ErrInvalid
			}
		}
	}
	return nil
}

// open places a start tag: the root, head and body, the body's outlines
// (recording candidates), anything else ignored (its subtree still counted).
func (doc *Document) open(t xml.StartElement, stack []frame, heads, bodies *int) (frame, error) {
	plain := t.Name.Space == ""
	if len(stack) == 0 {
		if !plain || t.Name.Local != "opml" {
			return frame{}, ErrInvalid
		}
		doc.Version = attr(t.Attr, "version")
		if doc.Version != "1.0" && doc.Version != "2.0" {
			return frame{}, ErrInvalid
		}
		return frame{kind: kindRoot}, nil
	}
	parent := stack[len(stack)-1]
	switch {
	case parent.kind == kindRoot && plain && t.Name.Local == "head":
		if *heads++; *heads > 1 {
			return frame{}, ErrInvalid
		}
		return frame{kind: kindHead}, nil
	case parent.kind == kindRoot && plain && t.Name.Local == "body":
		if *bodies++; *bodies > 1 {
			return frame{}, ErrInvalid
		}
		return frame{kind: kindBody}, nil
	case (parent.kind == kindBody || parent.kind == kindOutline) && plain && t.Name.Local == "outline":
		doc.Outlines++
		f := frame{kind: kindOutline, nesting: parent.nesting + 1}
		if doc.Outlines > MaxOutlines || f.nesting > MaxOutlineNesting {
			return frame{}, ErrLimit
		}
		url, has := xmlURL(t.Attr)
		if has || strings.EqualFold(strings.TrimSpace(attr(t.Attr, "type")), "rss") {
			doc.Candidates = append(doc.Candidates, Candidate{Index: doc.Outlines, URL: strings.TrimSpace(url)})
		}
		return f, nil
	}
	return frame{kind: kindOther}, nil
}

// attr is the unprefixed attribute name's value, "" when absent.
func attr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// xmlURL is the outline's xmlUrl in any letter case (checkAttrs allows one).
func xmlURL(attrs []xml.Attr) (string, bool) {
	for _, a := range attrs {
		if a.Name.Space == "" && strings.EqualFold(a.Name.Local, "xmlurl") {
			return a.Value, true
		}
	}
	return "", false
}
