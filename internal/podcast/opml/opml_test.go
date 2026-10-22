package opml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const fixtures = "../testdata/phase2c3/opml/"

func read(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixtures + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The shared parser expectations: valid lists give their feed outlines'
// indexes, every hostile or oversize sample its error, nothing expanded.
func TestParserExpectations(t *testing.T) {
	var cases []struct {
		File       string `json:"file"`
		HTTPStatus int    `json:"http_status"`
		Code       string `json:"code"`
		Indexes    []int  `json:"feed_outline_indexes"`
	}
	if err := json.Unmarshal(read(t, "parser-expectations.json"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.File, func(t *testing.T) {
			raw := read(t, c.File)
			if c.File == "huge.opml" {
				if len(raw) != MaxBytes+1 {
					t.Fatalf("huge is %d bytes", len(raw))
				}
				if _, err := ReadBody(bytes.NewReader(raw)); !errors.Is(err, ErrLimit) {
					t.Fatalf("ReadBody = %v", err)
				}
			}
			doc, err := Parse(raw)
			switch c.Code {
			case "":
				if err != nil {
					t.Fatal(err)
				}
				var got []int
				for _, cand := range doc.Candidates {
					got = append(got, cand.Index)
				}
				if fmt.Sprint(got) != fmt.Sprint(c.Indexes) {
					t.Fatalf("indexes %v, want %v", got, c.Indexes)
				}
			case "invalid_opml":
				if !errors.Is(err, ErrInvalid) || c.HTTPStatus != 400 {
					t.Fatalf("= %v", err)
				}
			case "opml_limit":
				if !errors.Is(err, ErrLimit) || c.HTTPStatus != 413 {
					t.Fatalf("= %v", err)
				}
			default:
				t.Fatalf("unknown code %s", c.Code)
			}
		})
	}
}

// The duplicates sample: categories count in the index, a type rss outline
// without xmlUrl is a (missing) candidate, an outline without either is not.
func TestParseDuplicatesSample(t *testing.T) {
	doc, err := Parse(read(t, "duplicates-private.opml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Candidate{
		{2, "https://feeds.example.org/one.xml"},
		{3, "HTTPS://Feeds.Example.org:443/one.xml#ignored"},
		{4, "https://feeds.example.org/old-one.xml"},
		{5, "https://feeds.example.org/private.xml?token=OPML_PRIVATE_CANARY_2C3&x=1"},
		{6, "file:///etc/hosts"},
		{7, ""},
	}
	if fmt.Sprint(doc.Candidates) != fmt.Sprint(want) || doc.Outlines != 8 || doc.Version != "2.0" {
		t.Fatalf("= %+v", doc)
	}
}

func wrap(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<opml version="2.0"><head><title>t</title></head><body>` + body + `</body></opml>`
}

func TestParseRules(t *testing.T) {
	many := func(n int, s string) string { return strings.Repeat(s, n) }
	attrs := ""
	for i := range 33 {
		attrs += fmt.Sprintf(` a%d="x"`, i)
	}
	for _, tc := range []struct {
		name, doc string
		want      error
		urls      []string
	}{
		{"bom", "\xEF\xBB\xBF" + wrap(`<outline xmlUrl="https://a.example/f"/>`), nil, []string{"https://a.example/f"}},
		{"no declaration", `<opml version="1.0"><body><outline xmlurl="https://a.example/f"/></body></opml>`, nil, []string{"https://a.example/f"}},
		{"escapes", wrap(`<outline xmlUrl="https://a.example/f?a=1&amp;b=&#x32;"/>`), nil, []string{"https://a.example/f?a=1&b=2"}},
		{"case alias alone", wrap(`<outline XMLURL="https://a.example/f"/>`), nil, []string{"https://a.example/f"}},
		{"empty xmlUrl", wrap(`<outline xmlUrl=" "/>`), nil, []string{""}},
		{"htmlUrl ignored", wrap(`<outline type="link" htmlUrl="https://a.example/" url="https://b.example/"/>`), nil, nil},
		{"RSS type", wrap(`<outline type="RSS"/>`), nil, []string{""}},
		{"extensions", `<?xml version="1.0"?><opml version="2.0" xmlns:x="urn:x"><x:meta/><head><x:y a="1"/></head><body><x:wrap><outline xmlUrl="https://hidden.example/"/></x:wrap><outline xmlUrl="https://a.example/f"/></body></opml>`, nil, []string{"https://a.example/f"}},
		{"comment", wrap(`<!-- c --><outline xmlUrl="https://a.example/f"/>`) + "\n<!-- tail -->\n", nil, []string{"https://a.example/f"}},
		{"single quoted declaration", `<?xml version='1.0' encoding='utf-8'?><opml version="1.0"><body/></opml>`, nil, nil},

		{"doctype unused", "<?xml version=\"1.0\"?>\n<!DOCTYPE opml>\n" + `<opml version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"latin1", `<?xml version="1.0" encoding="ISO-8859-1"?><opml version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"xml 1.1", `<?xml version="1.1"?><opml version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"invalid utf8", wrap("<outline text=\"\xff\"/>"), ErrInvalid, nil},
		{"late declaration", " " + wrap(""), ErrInvalid, nil},
		{"processing instruction", wrap(`<?php x?>`), ErrInvalid, nil},
		{"stylesheet", `<?xml version="1.0"?><?xml-stylesheet href="x"?><opml version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"unknown entity", wrap(`<outline text="&nbsp;" xmlUrl="https://a.example/f"/>`), ErrInvalid, nil},
		{"duplicate attribute", wrap(`<outline text="a" text="b"/>`), ErrInvalid, nil},
		{"xmlUrl aliases", wrap(`<outline xmlurl="https://a.example/1" xmlUrl="https://a.example/2"/>`), ErrInvalid, nil},
		{"namespaced root", `<opml xmlns="urn:x" version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"prefixed root", `<o:opml xmlns:o="urn:x" version="2.0"><body/></o:opml>`, ErrInvalid, nil},
		{"wrong root", `<rss version="2.0"><body/></rss>`, ErrInvalid, nil},
		{"no version", `<opml><body/></opml>`, ErrInvalid, nil},
		{"version 3", `<opml version="3.0"><body/></opml>`, ErrInvalid, nil},
		{"missing body", `<opml version="2.0"><head/></opml>`, ErrInvalid, nil},
		{"two bodies", `<opml version="2.0"><body/><body/></opml>`, ErrInvalid, nil},
		{"two heads", `<opml version="2.0"><head/><head/><body/></opml>`, ErrInvalid, nil},
		{"second root", wrap("") + `<opml version="2.0"><body/></opml>`, ErrInvalid, nil},
		{"text after root", wrap("") + "x", ErrInvalid, nil},
		{"unclosed", `<opml version="2.0"><body>`, ErrInvalid, nil},
		{"empty", ``, ErrInvalid, nil},
		{"structure error after a valid outline", wrap(`<outline xmlUrl="https://a.example/f"/></outline>`), ErrInvalid, nil},

		{"attributes", wrap(`<outline` + attrs + `/>`), ErrLimit, nil},
		{"attribute value", wrap(`<outline text="` + many(MaxAttributeBytes+1, "a") + `"/>`), ErrLimit, nil},
		{"elements in head", `<opml version="2.0"><head>` + many(MaxElements, "<x/>") + `</head><body/></opml>`, ErrLimit, nil},
		{"depth in head", `<opml version="2.0"><head>` + many(MaxDepth, "<x>") + many(MaxDepth, "</x>") + `</head><body/></opml>`, ErrLimit, nil},
		{"outlines", wrap(many(MaxOutlines+1, "<outline/>")), ErrLimit, nil},
		{"nesting", wrap(many(MaxOutlineNesting+1, "<outline>") + many(MaxOutlineNesting+1, "</outline>")), ErrLimit, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.doc))
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("= %v, want %v", err, tc.want)
			}
			if err != nil {
				return
			}
			var urls []string
			for _, c := range doc.Candidates {
				urls = append(urls, c.URL)
			}
			if fmt.Sprint(urls) != fmt.Sprint(tc.urls) {
				t.Fatalf("urls %q, want %q", urls, tc.urls)
			}
		})
	}
	// At the bounds exactly is fine.
	if _, err := Parse([]byte(wrap(many(MaxOutlineNesting, "<outline>") + many(MaxOutlineNesting, "</outline>")))); err != nil {
		t.Fatalf("16 nested = %v", err)
	}
	if _, err := Parse([]byte(wrap(many(MaxOutlines, "<outline/>")))); err != nil {
		t.Fatalf("1000 outlines = %v", err)
	}
	if _, err := ReadBody(bytes.NewReader(make([]byte, MaxBytes))); err != nil {
		t.Fatalf("exactly the cap = %v", err)
	}
}

// The export golden, byte for byte, and the rules behind it.
func TestRender(t *testing.T) {
	out, err := Render([]Entry{{Title: "Fixture One", URL: "https://feeds.example.org/one.xml"}})
	if err != nil || !bytes.Equal(out, read(t, "export-expected.opml")) {
		t.Fatalf("golden:\n%s %v", out, err)
	}
	empty, err := Render(nil)
	if err != nil || string(empty) != "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<opml version=\"2.0\">\n  <head><title>AudioSilo subscriptions</title></head>\n  <body>\n  </body>\n</opml>\n" {
		t.Fatalf("empty:\n%s", empty)
	}
	if _, err := Parse(empty); err != nil {
		t.Fatal(err)
	}
	out, _ = Render([]Entry{{Title: " \x01A <b> & \"q\"\n", URL: "https://a.example/f?x=1&y=2"}, {Title: "\x02 ", URL: "https://b.example/"}, {Title: strings.Repeat("é", 600), URL: "https://c.example/"}})
	doc, err := Parse(out)
	if err != nil || len(doc.Candidates) != 3 || doc.Candidates[0].URL != "https://a.example/f?x=1&y=2" {
		t.Fatalf("round trip %v %+v\n%s", err, doc, out)
	}
	if !strings.Contains(string(out), `text="A &lt;b&gt; &amp; &#34;q&#34;"`) || !strings.Contains(string(out), `text="Podcast"`) || !strings.Contains(string(out), `text="`+strings.Repeat("é", MaxTitleRunes)+`"`) {
		t.Fatalf("titles:\n%s", out)
	}
	big := make([]Entry, 20000)
	for i := range big {
		big[i] = Entry{Title: strings.Repeat("t", 200), URL: fmt.Sprintf("https://feeds.example.org/%d.xml", i)}
	}
	if _, err := Render(big); !errors.Is(err, ErrExportLimit) {
		t.Fatalf("over 4 MiB = %v", err)
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText(" a\x00b\u0085c\td ", 3); got != "abc" {
		t.Fatalf("%q", got)
	}
	if got := CleanText("\xff", 5); got != "�" {
		t.Fatalf("%q", got)
	}
}
