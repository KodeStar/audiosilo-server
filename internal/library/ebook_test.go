package library

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// epubBytes is a zip holding the named entries (empty bodies but container.xml's):
// minimalEpub's entries make the smallest epub the Bridge reads.
func epubBytes(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "META-INF/container.xml" {
			_, _ = w.Write([]byte(`<container><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`))
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var minimalEpub = []string{"mimetype", "META-INF/container.xml", "content.opf"}

// BookEbook finds what the Bridge's "auto" source reads: the one regular .epub
// directly in a folder book's folder (never a link, a hidden file, one deeper
// down; several is none), or "<stem>.epub" beside a single-file book (a link
// inside the library counts, as the Bridge follows it); never one outside the
// library.
func TestBookEbook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte("x")
		if strings.EqualFold(filepath.Ext(p), ".epub") {
			body = epubBytes(t, minimalEpub...)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Author/Book/01.m4b")
	write("Author/Book/Book.EPUB")
	write("Author/Book/Extras/other.epub")
	write("Author/Linked/01.m4b")
	write("Author/Hidden/01.m4b")
	write("Author/Hidden/.book.epub")
	write("Collection/One.m4b")
	write("Collection/One.epub")
	write("Collection/Two.m4b")
	write("Collection/Three.epub")
	write("Loose.mp3")
	write("Loose.epub")
	write("outside/Secret.epub")
	write("Author/Two/01.m4b")
	write("Author/Two/Two.epub")
	write("Author/Two/Two (Annotated).epub")
	write("Collection/Four.m4b")
	write("Collection/Five.m4b")
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Author", "Linked", "Linked.epub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Collection", "Four.epub")); err != nil {
		t.Fatal(err)
	}
	away := filepath.Join(t.TempDir(), "Away.epub")
	if err := os.WriteFile(away, epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, filepath.Join(root, "Collection", "Five.epub")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rel    string
		folder bool
		want   string
	}{
		{"Author/Book", true, "Author/Book/Book.EPUB"},
		{"Author/Linked", true, ""}, // a link is never followed
		{"Author/Hidden", true, ""}, // nor a hidden file
		{"Author/Two", true, ""},    // several: the Bridge reads the audio
		{"Collection/One.m4b", false, "Collection/One.epub"},
		{"Collection/Two.m4b", false, ""},                      // another book's epub doesn't count
		{"Collection/Four.m4b", false, "Collection/Four.epub"}, // a link inside the library
		{"Collection/Five.m4b", false, ""},                     // one leading out of it
		{"Loose.mp3", false, "Loose.epub"},
		{"Author/Missing", true, ""},
		{"../outside", true, ""},
	} {
		if got := BookEbook(root, tc.rel, tc.folder); got != tc.want {
			t.Errorf("BookEbook(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

// An .epub the Bridge would refuse to read (it falls back to the audio) isn't the
// estimate's ebook: one that isn't a zip, one with no META-INF/container.xml. A
// minimal valid one is, beside a single file and alone in a folder, and so is one
// carrying META-INF/encryption.xml (font obfuscation in a DRM-free book; the
// Bridge reads it too). A folder holding a readable and an unreadable one still
// holds several (the Bridge counts both).
func TestBookEbookReadable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	put := func(rel string, body []byte) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fonts := epubBytes(t, append(minimalEpub, "META-INF/encryption.xml")...)
	notZip := []byte("PK this is not a zip, only named like an epub")
	noContainer := epubBytes(t, "mimetype", "content.opf")
	valid := epubBytes(t, minimalEpub...)
	for name, body := range map[string][]byte{"Fonts": fonts, "NotZip": notZip, "NoContainer": noContainer, "Valid": valid} {
		put("Single/"+name+".m4b", []byte("x"))
		put("Single/"+name+".epub", body)
		put("Folder/"+name+"/01.m4b", []byte("x"))
		put("Folder/"+name+"/"+name+".epub", body)
	}
	put("Folder/Mixed/01.m4b", []byte("x"))
	put("Folder/Mixed/a.epub", valid)
	put("Folder/Mixed/b.epub", notZip)
	for _, tc := range []struct {
		rel    string
		folder bool
		want   string
	}{
		{"Single/Fonts.m4b", false, "Single/Fonts.epub"},
		{"Single/NotZip.m4b", false, ""},
		{"Single/NoContainer.m4b", false, ""},
		{"Single/Valid.m4b", false, "Single/Valid.epub"},
		{"Folder/Fonts", true, "Folder/Fonts/Fonts.epub"},
		{"Folder/NotZip", true, ""},
		{"Folder/NoContainer", true, ""},
		{"Folder/Valid", true, "Folder/Valid/Valid.epub"},
		{"Folder/Mixed", true, ""},
	} {
		if got := BookEbook(root, tc.rel, tc.folder); got != tc.want {
			t.Errorf("BookEbook(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

// diskFailing fails every read the way a disk does (os.File's *fs.PathError: a
// flaky mount, an I/O error).
type diskFailing struct{}

func (diskFailing) ReadAt([]byte, int64) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: "Book.epub", Err: errors.New("input/output error")}
}

// A read the disk fails is one that can't be done right now (an error: the scan
// looks again), never "not an epub", which would be recorded until the file
// changes; the archive's own faults are (false, nil).
func TestReadableEpubDiskError(t *testing.T) {
	t.Parallel()
	if ok, err := readableEpub(diskFailing{}, 4096); ok || err == nil {
		t.Errorf("a failing disk: readableEpub = %v, %v; want an error", ok, err)
	}
	for name, body := range map[string][]byte{
		"not a zip":    []byte(strings.Repeat("x", 64)),
		"no container": epubBytes(t, "mimetype", "content.opf"),
	} {
		if ok, err := readableEpub(bytes.NewReader(body), int64(len(body))); ok || err != nil {
			t.Errorf("%s: readableEpub = %v, %v; want false, nil", name, ok, err)
		}
	}
	valid := epubBytes(t, minimalEpub...)
	if ok, err := readableEpub(bytes.NewReader(valid), int64(len(valid))); !ok || err != nil {
		t.Errorf("a valid epub: readableEpub = %v, %v; want true, nil", ok, err)
	}
}

// OpenEbook opens only a regular file inside the library, the very one its name
// leads to: a link where none may be (noLink), one leading out of the library or
// to nothing, a hidden path and anything but a regular file are ErrNoEbook;
// nothing there is fs.ErrNotExist.
func TestOpenEbook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	writeFile(t, root, "A/Book.epub", epub)
	writeFile(t, root, "A/.hidden.epub", epub)
	away := filepath.Join(t.TempDir(), "Away.epub")
	if err := os.WriteFile(away, epub, 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"A/Inside.epub":   filepath.Join(root, "A", "Book.epub"),
		"A/Out.epub":      away,
		"A/Dangling.epub": filepath.Join(root, "A", "nothing.epub"),
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "A", "Dir.epub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rel    string
		noLink bool
		want   error
	}{
		{"A/Book.epub", true, nil},
		{"A/Book.epub", false, nil},
		{"A/Inside.epub", false, nil}, // a link inside the library counts...
		{"A/Inside.epub", true, ErrNoEbook},
		{"A/Out.epub", false, ErrNoEbook}, // ...one leading out never does
		{"A/Dangling.epub", false, ErrNoEbook},
		{"A/Dir.epub", false, ErrNoEbook},
		{"A/.hidden.epub", false, ErrNoEbook},
		{"../outside.epub", false, ErrNoEbook},
		{"A/Missing.epub", false, fs.ErrNotExist},
	} {
		f, info, err := OpenEbook(root, tc.rel, tc.noLink)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("OpenEbook(%q, noLink %v): %v, want it open", tc.rel, tc.noLink, err)
		case tc.want == nil && info.Size() != int64(len(epub)):
			t.Errorf("OpenEbook(%q): size %d, want %d", tc.rel, info.Size(), len(epub))
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("OpenEbook(%q, noLink %v): %v, want %v", tc.rel, tc.noLink, err, tc.want)
		}
		if f != nil {
			_ = f.Close()
		}
	}
}

// A single-file book's "<stem>.epub" spelled in another Unicode form (an NFD
// copy from macOS beside NFC audio) is its ebook for the reader, found by the
// walk's listing (ebookCandidate) and on demand (ebookOnDisk), in another case
// too where the disk opens the name so; a name that only looks alike without its
// accent is not (server#228). The estimate (BookEbook) keeps the Bridge's lookup
// of the name as spelled: it finds the epub only where the disk does, as the
// Bridge's os.Stat would (a disk that doesn't tell the forms apart, APFS; never
// ext4).
func TestBookEbookUnicodeForms(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	nfc, nfd := "Café", norm.NFD.String("Café")
	writeFile(t, root, "C/"+nfc+".m4b", []byte("x"))
	writeFile(t, root, "C/"+nfd+".epub", epub)
	writeFile(t, root, "C/Creme.m4b", []byte("x"))
	writeFile(t, root, "C/"+norm.NFD.String("Crème")+".epub", epub)
	// Another case and another form at once: the disk decides, as it does for
	// "Book.EPUB" (ebookNamed), on demand and in a scan alike.
	writeFile(t, root, "C/Soirée.m4b", []byte("x"))
	writeFile(t, root, "C/"+norm.NFD.String("SOIRÉE")+".epub", epub)
	entries, err := os.ReadDir(filepath.Join(root, "C"))
	if err != nil {
		t.Fatal(err)
	}
	var epubs []fs.DirEntry
	for _, e := range entries {
		if isEpubEntry(e) {
			epubs = append(epubs, e)
		}
	}
	onDemand := func(rel string) string {
		r, _ := checkEbook(root, ebookOnDisk(root, rel, false))
		return r.Path
	}
	scanned := func(rel string) string {
		r, _ := checkEbook(root, ebookCandidate(root, rel, false, epubs))
		return r.Path
	}
	opens := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, "C", name))
		return err == nil
	}

	if got := onDemand("C/" + nfc + ".m4b"); norm.NFC.String(got) != "C/Café.epub" {
		t.Errorf("on demand (NFC audio beside NFD epub) = %q, want C/Café.epub in some form", got)
	}
	if got := scanned("C/" + nfc + ".m4b"); norm.NFC.String(got) != "C/Café.epub" {
		t.Errorf("scanned (NFC audio beside NFD epub) = %q, want C/Café.epub in some form", got)
	}
	if got := BookEbook(root, "C/"+nfc+".m4b", false); (got != "") != opens(nfc+".epub") {
		t.Errorf("BookEbook(NFC audio beside NFD epub) = %q, want it only where the disk opens %q", got, nfc+".epub")
	}
	for _, find := range []func(string) string{onDemand, scanned, func(rel string) string { return BookEbook(root, rel, false) }} {
		if got := find("C/Creme.m4b"); got != "" {
			t.Errorf("Creme's ebook = %q, want none (Crème is another name)", got)
		}
	}
	want := ""
	if opens("Soirée.epub") {
		want = "C/Soirée.epub"
	}
	if a, b := onDemand("C/Soirée.m4b"), scanned("C/Soirée.m4b"); a != want || b != want {
		t.Errorf("another case and form: on demand %q, scanned %q, want %q both", a, b, want)
	}
	if ebookNameKey(nfd+".EPUB") != ebookNameKey(nfc+".epub") {
		t.Error("ebookNameKey differs across case and Unicode form")
	}
	if hasOtherForm("Don’t Panic.epub") || hasOtherForm("三体.epub") || !hasOtherForm(nfc+".epub") || !hasOtherForm(nfd+".epub") {
		t.Error("hasOtherForm: want true only for a name with another NFC/NFD spelling")
	}
}

// A file swapped for another as OpenEbook opens it is refused, never served
// (server#228); the same file opened is served.
func TestOpenEbookSwapped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	writeFile(t, root, "A/Book.epub", epub)
	writeFile(t, root, "A/Other.epub", epub)
	swap := true
	open := func(full string) (*os.File, error) {
		if swap {
			// Opened as the old file, then replaced: the name now leads to another.
			f, err := os.Open(full) //nolint:gosec // test
			if err != nil {
				return nil, err
			}
			if err := os.Rename(filepath.Join(root, "A", "Other.epub"), full); err != nil {
				t.Fatal(err)
			}
			return f, nil
		}
		return os.Open(full) //nolint:gosec // test
	}
	if f, _, err := openEbook(root, "A/Book.epub", true, open); !errors.Is(err, ErrNoEbook) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("swapped open = %v, want ErrNoEbook", err)
	}
	swap = false
	f, _, err := openEbook(root, "A/Book.epub", true, open)
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	_ = f.Close()
}
