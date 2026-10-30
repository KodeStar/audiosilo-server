package upcoming

import (
	"bytes"
	"cmp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The calendar feed (Track U3): GET /me/upcoming/releases.ics is the caller's
// Answer as an RFC 5545 calendar, one all-day event per preorder or recent
// release with a day-precision date. A missing volume is left out (its date is a
// back-catalogue first release, often years past), and so is a date known only
// to the year or month (no honest all-day event). Each event's UID is the work's
// id at the server's, so a date that slips moves the event instead of adding one.
// With no such event the calendar has no component at all: RFC 5545 asks for
// one, and calendar apps read it as an empty calendar.

// Calendar is what the feed says about itself besides the items.
type Calendar struct {
	// ServerID makes the events' UIDs (<work id>@<server id>); "" is
	// "audiosilo".
	ServerID string
	// ServerName is added to the calendar's name when the server has one.
	ServerName string
	// Now stamps the events when the answer has no generated_at.
	Now time.Time
}

// The calendar's fixed properties.
const (
	icsProdID   = "-//AudioSilo//Upcoming releases//EN"
	icsName     = "AudioSilo: upcoming releases"
	icsRefresh  = "PT12H"
	icsMaxOctet = 75 // a content line's octets before it is folded (RFC 5545 3.1)
)

// ICS renders ans as an iCalendar document (CRLF line ends, folded at 75
// octets without splitting a UTF-8 sequence, TEXT values escaped).
func ICS(ans Answer, c Calendar) []byte {
	var w icsWriter
	name := icsName
	if c.ServerName != "" {
		name += " (" + c.ServerName + ")"
	}
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:" + icsProdID)
	w.line("CALSCALE:GREGORIAN")
	w.line("X-WR-CALNAME:" + icsText(name))
	w.line("REFRESH-INTERVAL;VALUE=DURATION:" + icsRefresh)
	w.line("X-PUBLISHED-TTL:" + icsRefresh)
	stamp := c.Now
	if t, ok := parseStamp(ans.GeneratedAt); ok {
		stamp = t
	}
	dtstamp := stamp.UTC().Format("20060102T150405Z")
	server := cmp.Or(c.ServerID, "audiosilo")
	for _, it := range ans.Items {
		if it.Kind != catalog.UpcomingPreorder && it.Kind != catalog.UpcomingReleased {
			continue
		}
		day, ok := icsDay(it.ReleaseDate)
		if !ok {
			continue
		}
		w.line("BEGIN:VEVENT")
		w.line("UID:" + icsText(it.Work.ID+"@"+server))
		w.line("DTSTAMP:" + dtstamp)
		w.line("DTSTART;VALUE=DATE:" + day.Format("20060102"))
		w.line("DTEND;VALUE=DATE:" + day.AddDate(0, 0, 1).Format("20060102"))
		rel := Release{WorkID: it.Work.ID, Title: it.Work.Title, Series: it.Series.Name, Position: it.Series.Position}
		w.line("SUMMARY:" + icsText(rel.Label()))
		w.line("DESCRIPTION:" + icsText(eventDescription(it)))
		if u := icsURI(it.Work.WebURL); u != "" {
			w.line("URL:" + u)
		}
		w.line("TRANSP:TRANSPARENT")
		w.line("END:VEVENT")
	}
	w.line("END:VCALENDAR")
	return w.buf.Bytes()
}

// icsDay is a day-precision release date (YYYY-MM-DD, read as
// parseReleaseDate reads it) whose next day, the event's end, is still a
// four-digit year (RFC 5545 date-fullyear); false for any other.
func icsDay(date string) (time.Time, bool) {
	t, ok := parseReleaseDate(date)
	return t, ok && len(date) == len(time.DateOnly) && t.AddDate(0, 0, 1).Year() <= 9999
}

// eventDescription is the authors, the first release date and the work's
// community page, one to a line.
func eventDescription(it Item) string {
	var lines, names []string
	for _, a := range it.Work.Authors {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	if len(names) > 0 {
		lines = append(lines, "By "+strings.Join(names, ", "))
	}
	lines = append(lines, "First release: "+it.ReleaseDate)
	if it.Work.WebURL != "" {
		lines = append(lines, it.Work.WebURL)
	}
	return strings.Join(lines, "\n")
}

// icsBreaks makes every line break (CRLF, a lone CR or LF) one LF.
var icsBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// icsText escapes a TEXT value (RFC 5545 3.3.11): backslash, semicolon and
// comma escaped, a line break as \n, any other control character and any
// invalid UTF-8 dropped.
func icsText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range icsBreaks.Replace(strings.ToValidUTF8(s, "")) {
		switch {
		case r == '\\' || r == ';' || r == ',':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// a control character has no place in TEXT
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// icsURI is a URI value as it is, or "" when it holds a character no content
// line may (a control character or a space): never escaped, so never trusted
// blindly either.
func icsURI(s string) string {
	for _, r := range s {
		if r <= 0x20 || r == 0x7f || r == utf8.RuneError {
			return ""
		}
	}
	return s
}

// icsWriter writes content lines with CRLF ends, folding each at icsMaxOctet
// octets (a continuation line starts with one space, which counts) and never
// inside a UTF-8 sequence.
type icsWriter struct{ buf bytes.Buffer }

func (w *icsWriter) line(s string) {
	limit := icsMaxOctet
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		w.buf.WriteString(s[:cut])
		w.buf.WriteString("\r\n ")
		s = s[cut:]
		limit = icsMaxOctet - 1
	}
	w.buf.WriteString(s)
	w.buf.WriteString("\r\n")
}
