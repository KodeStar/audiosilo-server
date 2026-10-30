package upcoming

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// icsAnswer is a list holding one item of every shape the calendar meets: the
// two kinds it shows with a day-precision date, and the ones it leaves out (a
// missing volume, a month or year date, no date, a date that isn't one, a day
// whose next day has a five-digit year).
func icsAnswer() Answer {
	work := func(id, title string) ItemWork {
		return ItemWork{ID: id, Title: title, WebURL: "https://meta.audiosilo.app/work?id=" + id}
	}
	series := ItemSeries{ID: "expanse", Name: "The Expanse", Position: "4"}
	return Answer{Status: StatusOK, GeneratedAt: "2026-10-20T04:00:00.000Z", WindowDays: 90, Items: []Item{
		{Kind: catalog.UpcomingPreorder, ReleaseDate: "2099-11-12", Work: ItemWork{
			ID: "e4", Title: `Cibola Burn; or, Back\slash`, WebURL: "https://meta.audiosilo.app/work?id=e4",
			Authors: []meta.MetaPersonRef{{ID: "a", Name: "James S. A. Corey"}, {ID: "b", Name: "Ty Franck"}},
		}, Series: series},
		{Kind: catalog.UpcomingReleased, ReleaseDate: "2026-10-01", Work: work("e3",
			"Abaddon’s Gate: a very long title that runs past the fold with naïve façades, 日本語のタイトル and more"),
			Series: ItemSeries{ID: "expanse", Name: "The Expanse", Position: "3"}},
		{Kind: catalog.UpcomingReleased, ReleaseDate: "2026-09-30", Work: work("x1", "Two\r\nLines"), Series: ItemSeries{}},
		{Kind: catalog.UpcomingPreorder, ReleaseDate: "2098-01", Work: work("e5", "Month only"), Series: series},
		{Kind: catalog.UpcomingPreorder, ReleaseDate: "2097", Work: work("e6", "Year only"), Series: series},
		{Kind: catalog.UpcomingReleased, Work: work("e7", "Undated"), Series: series},
		{Kind: catalog.UpcomingReleased, ReleaseDate: "2026-13-45", Work: work("e8", "Not a date"), Series: series},
		{Kind: catalog.UpcomingPreorder, ReleaseDate: "9999-12-31", Work: work("e9", "The last day"), Series: series},
		{Kind: catalog.UpcomingMissing, ReleaseDate: "2012-05-01", Work: work("e2", "Caliban's War"), Series: series},
	}}
}

func TestICSGolden(t *testing.T) {
	t.Parallel()
	got := ICS(icsAnswer(), Calendar{ServerID: "srv-1", ServerName: "Den, upstairs", Now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)})
	want, err := os.ReadFile("testdata/releases.ics")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("calendar differs from the golden:\n%s", got)
	}
	checkICSLines(t, got)
	for _, left := range []string{"e5@", "e6@", "e7@", "e8@", "e9@", "e2@"} {
		if bytes.Contains(got, []byte("UID:"+left)) {
			t.Errorf("%s must be left out", left)
		}
	}
}

// checkICSLines is RFC 5545's line rules: every line ends in CRLF, holds at
// most 75 octets, and is valid UTF-8 on its own (a fold never splits a
// sequence); unfolded, no line holds a raw control character.
func checkICSLines(t *testing.T, doc []byte) {
	t.Helper()
	s := string(doc)
	if !strings.HasSuffix(s, "\r\n") {
		t.Fatal("the document must end with CRLF")
	}
	for i, l := range strings.Split(strings.TrimSuffix(s, "\r\n"), "\r\n") {
		if strings.ContainsAny(l, "\r\n") {
			t.Errorf("line %d holds a bare CR or LF: %q", i, l)
		}
		if len(l) > icsMaxOctet {
			t.Errorf("line %d is %d octets: %q", i, len(l), l)
		}
		if !utf8.ValidString(l) {
			t.Errorf("line %d splits a UTF-8 sequence: %q", i, l)
		}
	}
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n ", ""), "\r\n") {
		for _, r := range l {
			if r < 0x20 && r != '\t' {
				t.Errorf("unfolded line holds a control character: %q", l)
			}
		}
	}
}

func TestICSEmptyPending(t *testing.T) {
	t.Parallel()
	got := string(ICS(Answer{Status: StatusPending, Items: []Item{}}, Calendar{Now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}))
	want := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//AudioSilo//Upcoming releases//EN\r\nCALSCALE:GREGORIAN\r\n" +
		"X-WR-CALNAME:AudioSilo: upcoming releases\r\nREFRESH-INTERVAL;VALUE=DURATION:PT12H\r\n" +
		"X-PUBLISHED-TTL:PT12H\r\nEND:VCALENDAR\r\n"
	if got != want {
		t.Fatalf("empty calendar = %q", got)
	}
}

// No generated_at: the events are stamped now; no server id: "audiosilo".
func TestICSStampAndUIDFallbacks(t *testing.T) {
	t.Parallel()
	ans := Answer{Items: []Item{{Kind: catalog.UpcomingPreorder, ReleaseDate: "2099-12-31", Work: ItemWork{ID: "w1", Title: "T"}}}}
	got := string(ICS(ans, Calendar{Now: time.Date(2026, 10, 30, 12, 34, 56, 0, time.FixedZone("x", 3600))}))
	for _, want := range []string{"UID:w1@audiosilo\r\n", "DTSTAMP:20261030T113456Z\r\n", "DTSTART;VALUE=DATE:20991231\r\n",
		"DTEND;VALUE=DATE:21000101\r\n", "SUMMARY:T\r\n", "DESCRIPTION:First release: 2099-12-31\r\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("calendar lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "URL:") {
		t.Errorf("no web_url, no URL property:\n%s", got)
	}
}

func TestICSFold(t *testing.T) {
	t.Parallel()
	fold := func(s string) string {
		var w icsWriter
		w.line(s)
		return w.buf.String()
	}
	exact := strings.Repeat("a", 75)
	if got := fold(exact); got != exact+"\r\n" {
		t.Fatalf("75 octets must not fold: %q", got)
	}
	if got := fold(exact + "b"); got != exact+"\r\n b\r\n" {
		t.Fatalf("76 octets = %q", got)
	}
	// A two-octet rune straddling octet 75 moves to the next line whole; the
	// continuation's leading space counts toward its 75.
	s := strings.Repeat("a", 74) + "é" + strings.Repeat("b", 80)
	got := fold(s)
	want := strings.Repeat("a", 74) + "\r\n é" + strings.Repeat("b", 72) + "\r\n " + strings.Repeat("b", 8) + "\r\n"
	if got != want {
		t.Fatalf("fold = %q\nwant %q", got, want)
	}
	checkICSLines(t, []byte(got))
	// Three-octet runes only: every line still valid UTF-8 and within 75.
	checkICSLines(t, []byte(fold(strings.Repeat("語", 200))))
}

func TestICSText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`a\b`:        `a\\b`,
		"a;b,c":      `a\;b\,c`,
		"one\ntwo":   `one\ntwo`,
		"one\r\ntwo": `one\ntwo`,
		"one\rtwo":   `one\ntwo`,
		"keep\ufffd": "keep\ufffd",
		"bell\x07":   "bell",
		"tab\there":  "tab\there",
		"café 日本":    "café 日本",
		"bad\xffutf": "badutf",
	} {
		if got := icsText(in); got != want {
			t.Errorf("icsText(%q) = %q, want %q", in, got, want)
		}
	}
	if icsURI("https://x/y?a=1") != "https://x/y?a=1" || icsURI("https://x/\r\nX-EVIL:1") != "" || icsURI("https://x/a b") != "" {
		t.Error("icsURI must pass a clean URI and drop one with a control character or space")
	}
}
