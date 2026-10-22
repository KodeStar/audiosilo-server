package opml

import (
	"bytes"
	"encoding/xml"
	"errors"
)

// MaxExportBytes bounds a rendered export.
const MaxExportBytes = 4 << 20

// ErrExportLimit is an export that would pass MaxExportBytes.
var ErrExportLimit = errors.New("OPML export over its limit")

// Entry is one exported subscription: a public, normalized feed URL and its
// show's title (cleaned by Render).
type Entry struct {
	Title string
	URL   string
}

// MaxTitleRunes bounds an exported title.
const MaxTitleRunes = 500

// Render writes the export in the fixed form the shared golden pins: an XML
// declaration, OPML 2.0, LF line ends and two-space indents, the head's fixed
// title and one flat outline per entry (type, text, title, xmlUrl in that
// order, paired end tags), in the order given. No date, user or server name.
// A title cleaned to nothing is "Podcast".
func Render(entries []Entry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<opml version=\"2.0\">\n  <head><title>AudioSilo subscriptions</title></head>\n  <body>\n")
	for _, e := range entries {
		title := CleanText(e.Title, MaxTitleRunes)
		if title == "" {
			title = "Podcast"
		}
		b.WriteString(`    <outline type="rss" text="`)
		escape(&b, title)
		b.WriteString(`" title="`)
		escape(&b, title)
		b.WriteString(`" xmlUrl="`)
		escape(&b, e.URL)
		b.WriteString("\"></outline>\n")
		if b.Len() > MaxExportBytes {
			return nil, ErrExportLimit
		}
	}
	b.WriteString("  </body>\n</opml>\n")
	if b.Len() > MaxExportBytes {
		return nil, ErrExportLimit
	}
	return b.Bytes(), nil
}

func escape(b *bytes.Buffer, s string) { _ = xml.EscapeText(b, []byte(s)) }
