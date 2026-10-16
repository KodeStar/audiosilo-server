package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The file a user posted (ABS 2.x, "Store metadata with item"), trimmed of its
// chapters.
const absSample = `{
  "tags": ["Growing Up & Facts of Life", "Historical Fiction"],
  "chapters": [{"id": 0, "start": 0, "end": 1244, "title": "Tuesday, May 18, 1954"}],
  "title": "With the Might of Angels (Dear America)",
  "subtitle": "With the Might of Angels",
  "authors": ["Andrea Davis Pinkney"],
  "narrators": ["Channie Waites"],
  "series": [],
  "genres": ["Children's Audiobooks"],
  "publishedYear": "2011",
  "publishedDate": null,
  "publisher": "Scholastic Audio Books",
  "description": "<p>In the fall of 1955, twelve-year-old Dawn Rae Johnson learns she will be the only one of her friends attending a previously all-white school. </p>",
  "isbn": "9780545437059",
  "asin": "B005LXU2TA",
  "language": "English",
  "explicit": false,
  "abridged": false
}`

func TestParseABSFile(t *testing.T) {
	t.Parallel()
	got, err := ParseABSFile([]byte(absSample))
	if err != nil {
		t.Fatal(err)
	}
	want := &ABSFile{
		Title:     "With the Might of Angels (Dear America)",
		Subtitle:  "With the Might of Angels",
		Author:    "Andrea Davis Pinkney",
		Narrator:  "Channie Waites",
		Published: "2011",
		Description: "In the fall of 1955, twelve-year-old Dawn Rae Johnson learns she will be the only " +
			"one of her friends attending a previously all-white school.",
		Publisher: "Scholastic Audio Books",
		Language:  "English",
		Genres:    []string{"Children's Audiobooks"},
		Tags:      []string{"Growing Up & Facts of Life", "Historical Fiction"},
		ASIN:      "B005LXU2TA",
		ISBN:      "9780545437059",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseABSFile =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseABSFileSeriesAndPeople(t *testing.T) {
	t.Parallel()
	got, err := ParseABSFile([]byte(`{
		"authors": ["Terry Pratchett", " ", {"name": "Neil Gaiman"}],
		"narrators": [{"name": "Stephen Briggs"}, 7],
		"series": ["Discworld #4", "Death #1", "Standalone", "Odd # Name #1.5", {"name": "Obj", "sequence": "2"}, {"name": "Num", "sequence": 3}, ""],
		"publishedDate": "1987-11-12",
		"publishedYear": "1987"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Author != "Terry Pratchett, Neil Gaiman" || got.Narrator != "Stephen Briggs" {
		t.Errorf("people = %q / %q", got.Author, got.Narrator)
	}
	wantSeries := []ABSSeries{
		{"Discworld", "4"}, {"Death", "1"}, {"Standalone", ""}, {"Odd # Name", "1.5"},
		{"Obj", "2"}, {"Num", "3"},
	}
	if !reflect.DeepEqual(got.Series, wantSeries) {
		t.Errorf("series = %+v, want %+v", got.Series, wantSeries)
	}
	if got.Published != "1987-11-12" {
		t.Errorf("published = %q, want the date over the year", got.Published)
	}
}

// The label lists and the abridged flag: an item that isn't a string costs that
// item, and abridged says something only as true.
func TestParseABSFileLabels(t *testing.T) {
	t.Parallel()
	got, err := ParseABSFile([]byte(`{"genres": ["Fantasy", " ", 3, " Epic "], "tags": "not a list", "abridged": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Genres, []string{"Fantasy", "Epic"}) || got.Tags != nil || !got.Abridged {
		t.Errorf("labels = %q / %q, abridged %v", got.Genres, got.Tags, got.Abridged)
	}
	if got, _ := ParseABSFile([]byte(`{"abridged": "yes"}`)); got.Abridged {
		t.Error("a string abridged should say nothing")
	}
}

// A value of the wrong type costs that value, not the file.
func TestParseABSFileLenient(t *testing.T) {
	t.Parallel()
	got, err := ParseABSFile([]byte(`{"title": {"x": 1}, "authors": "Not A List", "series": {"a": 1}, "asin": "B000000001", "publishedYear": 1999}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "" || got.Author != "" || got.Series != nil || got.ASIN != "B000000001" || got.Published != "1999" {
		t.Errorf("lenient parse = %+v", got)
	}
	if _, err := ParseABSFile([]byte(`not json`)); err == nil {
		t.Error("a file that isn't JSON should fail")
	}
}

func TestReadABSFileTooLarge(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), ABSFileName)
	if err := os.WriteFile(p, []byte(`{"title":"`+strings.Repeat("x", maxABSFile)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadABSFile(p); err != ErrABSFileTooLarge || !errors.Is(err, ErrABSFileInvalid) {
		t.Errorf("err = %v, want ErrABSFileTooLarge (invalid)", err)
	}
}

// TestReadABSFileKinds: a file that isn't JSON or isn't a regular file is
// unusable (ErrABSFileInvalid: no use reading it again until it changes); one that
// isn't there is not (a later read may work); a byte order mark is skipped.
func TestReadABSFileKinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if f, err := ReadABSFile(write("bom.json", "\xef\xbb\xbf"+`{"asin":"B000000001"}`)); err != nil || f.ASIN != "B000000001" {
		t.Errorf("with a byte order mark: %+v, %v", f, err)
	}
	if _, err := ReadABSFile(write("bad.json", "not json")); !errors.Is(err, ErrABSFileInvalid) {
		t.Errorf("not JSON: err = %v, want ErrABSFileInvalid", err)
	}
	if _, err := ReadABSFile(filepath.Join(dir, "missing.json")); err == nil || errors.Is(err, ErrABSFileInvalid) {
		t.Errorf("missing: err = %v, want a read error that isn't ErrABSFileInvalid", err)
	}
	// A link is never followed, even to a well-formed file.
	target := write("target.json", `{"asin":"B000000002"}`)
	link := filepath.Join(dir, ABSFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if f, err := ReadABSFile(link); !errors.Is(err, ErrABSFileInvalid) {
		t.Errorf("a link: %+v, err = %v, want ErrABSFileInvalid", f, err)
	}
}

func TestHTMLText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"<p>One &amp; two.</p><p>Three<br/>four</p>": "One & two.\nThree\nfour",
		"plain text":               "plain text",
		"<p>a</p>\n\n\n\n<p>b</p>": "a\n\nb",
		"<ul><li>x</li><li>y</li></ul>  <b>bold</b> end": "x\ny\nbold end",
	} {
		if got := htmlText(in); got != want {
			t.Errorf("htmlText(%q) = %q, want %q", in, got, want)
		}
	}
}
