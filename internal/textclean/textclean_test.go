package textclean

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := []struct {
		in    string
		max   int
		lines bool
		want  string
	}{
		{"plain", 10, false, "plain"},
		{"a\x00b\x7fc\u0085d\te", 20, false, "a b c d e"}, // C0, DEL, C1 and a tab: spaces
		{"one\ntwo\r\n", 20, true, "one\ntwo \n"},         // lines keeps the line breaks only
		{"one\ntwo", 20, false, "one two"},                // one line
		{"\xffok", 10, false, "�ok"},                      // a broken sequence
		{"ééé", 5, false, "éé"},                           // cut at a character, not inside one
		{"  spaced  ", 20, false, "  spaced  "},           // Clean keeps the spaces
		{"abc", 0, false, ""},
	}
	for _, c := range cases {
		if got := Clean(c.in, c.max, c.lines); got != c.want {
			t.Errorf("Clean(%q, %d, %v) = %q, want %q", c.in, c.max, c.lines, got, c.want)
		}
	}
}

func TestLine(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{" \x01A <b>\n", 20, "A <b>"},
		{"   " + strings.Repeat("x", 5), 5, "xxxxx"}, // the bound counts the text, not the spaces before it
		{"ab cd", 3, "ab"},                           // nothing trailing left by the cut
		{"\x02 ", 10, ""},
		{strings.Repeat("é", 600), 1000, strings.Repeat("é", 500)},
	}
	for _, c := range cases {
		if got := Line(c.in, c.max); got != c.want {
			t.Errorf("Line(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "  ", " b ", "c"); got != "b" {
		t.Fatalf("got %q, want the first non-blank, trimmed", got)
	}
	if got := FirstNonEmpty(" ", ""); got != "" {
		t.Fatalf("all blank = %q", got)
	}
	if got := FirstNonEmpty(); got != "" {
		t.Fatalf("none = %q", got)
	}
}
