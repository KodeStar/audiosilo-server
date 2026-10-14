package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// ABSFileName is the file Audiobookshelf keeps in a book's folder when its
// "Store metadata with item" setting is on: what ABS knows about the book, its
// matches and its users' corrections included, which the audio's tags often
// don't carry (an ASIN or ISBN above all).
const ABSFileName = "metadata.json"

// maxABSFile bounds a metadata.json read: a real one is a few KB (a long book's
// chapter list a few hundred), so anything near this is not ABS's.
const maxABSFile = 4 << 20

// ErrABSFileInvalid marks a metadata.json that was there but can't be used (not
// JSON, too large, not a regular file): reading it again won't help until it
// changes. Any other error from ReadABSFile means it couldn't be read right now
// (gone since it was found, a flaky mount), so a later read may succeed.
var ErrABSFileInvalid = errors.New("unusable " + ABSFileName)

// ErrABSFileTooLarge is a metadata.json over maxABSFile, left unread.
var ErrABSFileTooLarge = fmt.Errorf("%w: too large", ErrABSFileInvalid)

// errABSFileNotRegular is a metadata.json that is a link or anything else but a
// regular file, left unread.
var errABSFileNotRegular = fmt.Errorf("%w: not a regular file", ErrABSFileInvalid)

// utf8BOM is the byte order mark a Windows editor may put before the JSON.
var utf8BOM = []byte("\xef\xbb\xbf")

// ABSFile is what an Audiobookshelf metadata.json says about a book, "" (nil)
// for whatever it leaves out. Values are as the file has them, trimmed; the
// catalog checks each against its field's rules.
type ABSFile struct {
	Title       string
	Author      string // the file's authors, joined with ", "
	Narrator    string // likewise
	Series      []ABSSeries
	Published   string // YYYY[-MM[-DD]] (ReleaseDate) from publishedDate, else publishedYear
	Description string // plain text: ABS keeps HTML
	ASIN        string
	ISBN        string
}

// ABSSeries is one of the book's series in an ABS file, in the file's order (ABS
// lists the book's own first), with its position as written ("" for none).
type ABSSeries struct {
	Name     string
	Position string
}

// absJSON is the part of ABS's metadata.json (Book.toJSONForMetadataFile, ABS
// 2.3 onwards) AudioSilo reads. Every value is kept raw, so one of an unexpected
// type costs that value (absScalar, absList), never the file. People and series
// are strings there; the object forms ABS uses elsewhere ({name},
// {name, sequence}) are taken too.
type absJSON struct {
	Title         json.RawMessage `json:"title"`
	Authors       json.RawMessage `json:"authors"`
	Narrators     json.RawMessage `json:"narrators"`
	Series        json.RawMessage `json:"series"`
	PublishedYear json.RawMessage `json:"publishedYear"`
	PublishedDate json.RawMessage `json:"publishedDate"`
	Description   json.RawMessage `json:"description"`
	ISBN          json.RawMessage `json:"isbn"`
	ASIN          json.RawMessage `json:"asin"`
}

// ABSSignature is what tells one metadata.json from its next version: its size
// and modification time. ABS rewrites the file without touching the audio, so a
// scan compares this, not the book's own signature.
func ABSSignature(info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// ReadABSFile reads the ABS metadata.json at path (ParseABSFile): a regular file
// only, never through a link, including one swapped in after the file was looked
// at, so a link can't make it read a file outside the library. An error wrapping
// ErrABSFileInvalid is about the file itself; any other is a read that may work
// later.
func ReadABSFile(path string) (*ABSFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errABSFileNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil {
		return nil, err
	} else if !os.SameFile(info, opened) {
		// Replaced between the two looks (ABS renaming a new version into place, or
		// a link): the next read sees what is there then.
		return nil, fmt.Errorf("%s: replaced while being opened", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxABSFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxABSFile {
		return nil, ErrABSFileTooLarge
	}
	return ParseABSFile(data)
}

// ParseABSFile reads an ABS metadata.json. It is lenient about anything it
// doesn't use; a value of an unexpected type is left out rather than failing the
// file, and a leading byte order mark is skipped. A file that isn't JSON is
// ErrABSFileInvalid.
func ParseABSFile(data []byte) (*ABSFile, error) {
	var j absJSON
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &j); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrABSFileInvalid, err)
	}
	out := &ABSFile{
		Title:       absScalar(j.Title),
		Author:      strings.Join(absNames(j.Authors), ", "),
		Narrator:    strings.Join(absNames(j.Narrators), ", "),
		Published:   firstReleaseDate(absScalar(j.PublishedDate), absScalar(j.PublishedYear)),
		Description: htmlText(absScalar(j.Description)),
		ASIN:        absScalar(j.ASIN),
		ISBN:        absScalar(j.ISBN),
	}
	for _, s := range absList(j.Series) {
		if ref, ok := absSeries(s); ok {
			out.Series = append(out.Series, ref)
		}
	}
	return out, nil
}

// absList is a JSON list's items (none for anything else).
func absList(v json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	_ = json.Unmarshal(v, &items)
	return items
}

// absNames is the names in a list of people, each a string or a {name} object.
func absNames(list json.RawMessage) []string {
	var out []string
	for _, item := range absList(list) {
		var name string
		if json.Unmarshal(item, &name) != nil {
			var obj struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(item, &obj) != nil {
				continue
			}
			name = obj.Name
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// absSeries reads one series: ABS writes "Name #sequence" (the name alone with
// no sequence); a {name, sequence} object is taken too.
func absSeries(item json.RawMessage) (ABSSeries, bool) {
	var s string
	if json.Unmarshal(item, &s) == nil {
		s = strings.TrimSpace(s)
		ref := ABSSeries{Name: s}
		if i := strings.LastIndex(s, " #"); i > 0 {
			ref = ABSSeries{Name: strings.TrimSpace(s[:i]), Position: strings.TrimSpace(s[i+2:])}
		}
		return ref, ref.Name != ""
	}
	var obj struct {
		Name     string          `json:"name"`
		Sequence json.RawMessage `json:"sequence"`
	}
	if json.Unmarshal(item, &obj) != nil {
		return ABSSeries{}, false
	}
	ref := ABSSeries{Name: strings.TrimSpace(obj.Name), Position: absScalar(obj.Sequence)}
	return ref, ref.Name != ""
}

// absScalar is a JSON string or number as text, trimmed ("" for null or anything
// else): ABS writes strings, but a hand-edited file may write a year or an ISBN
// as a number.
func absScalar(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(v, &n) == nil {
		return n.String()
	}
	return ""
}

var (
	htmlBreakRE = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6])\s*>`)
	htmlTagRE   = regexp.MustCompile(`<[^>]*>`)
	blankRunRE  = regexp.MustCompile(`\n{3,}`)
)

// htmlText is an ABS description (HTML from its editor or a match) as plain
// text: a paragraph or line break becomes a line break, other tags go, entities
// are decoded and each line is trimmed.
func htmlText(s string) string {
	s = htmlBreakRE.ReplaceAllString(s, "\n")
	s = html.UnescapeString(htmlTagRE.ReplaceAllString(s, ""))
	s = strings.ReplaceAll(s, "\r", "")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(blankRunRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
