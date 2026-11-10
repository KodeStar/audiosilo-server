package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// testReadalong is a valid read-along map made from an ebook with digest sha.
func testReadalong(sha string) string {
	return fmt.Sprintf(`{"format":"audiosilo-readalong/v1","ebook":{"file":"Book.epub","sha256":%q},
		"audio":{"duration":1,"files":[{"path":"01.m4b","duration":1}]},
		"coverage":{"text":0.9,"audio":0.8},"sections":[]}`, sha)
}

// digestOf is the lowercase hex sha256 of data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestAttachmentsSigWithReadalong: readalong.json is listed last, so a book
// without one keeps the signature it had before the name was known, byte for
// byte.
func TestAttachmentsSigWithReadalong(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Unix(1700000000, 0)
	for _, name := range []string{"characters.json", "bridge.json", "readalong.json", "Loose.readalong.json"} {
		writeAt(t, dir, name, "{}", at)
	}
	files, err := listAttachments(dir)
	if err != nil {
		t.Fatal(err)
	}
	ns := at.UnixNano()
	without := fmt.Sprintf("characters.json:2:%d,bridge.json:2:%d", ns, ns)
	withRA := without + fmt.Sprintf(",readalong.json:2:%d", ns)
	if got := attachmentsSig(files, ""); got != withRA {
		t.Errorf("signature = %q, want %q", got, withRA)
	}
	delete(files, "readalong.json")
	if got := attachmentsSig(files, ""); got != without {
		t.Errorf("without a map = %q, want the signature unchanged, %q", got, without)
	}
	if got := attachmentsSig(files, "Loose."); got != fmt.Sprintf("readalong.json:2:%d", ns) {
		t.Errorf("a single-file book's = %q", got)
	}
}

// TestScannerReadsReadalong: the scan stores a map's checked head (a folder
// book's, a single-file book's by its stem) and hashes the ebook of a book with
// a map only, again once the ebook changes (with no re-index); a map that fails
// its checks is none, with its signature recorded.
func TestScannerReadsReadalong(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	s := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	past := time.Now().Add(-time.Hour)
	epub := epubBytes(t, minimalEpub...)
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "B", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Loose.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Bad.m4b"))
	for _, rel := range []string{"A/Book/Book.epub", "B/Book/Book.epub", "Loose.epub", "Bad.epub"} {
		writeFile(t, root, rel, epub)
	}
	writeAt(t, root, "A/Book/.audiosilo/readalong.json", testReadalong(digestOf(epub)), past)
	writeAt(t, root, ".audiosilo/Loose.readalong.json", testReadalong(digestOf(epub)), past)
	writeAt(t, root, ".audiosilo/Bad.readalong.json", `{"format":"audiosilo-readalong/v9"}`, past)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"A/Book", "Loose.m4b"} {
		got, err := cat.BookReadalongAt(ctx, lib.ID, rel)
		if err != nil || got.Head == nil || !got.DigestCurrent || got.EbookSHA256 != digestOf(epub) {
			t.Errorf("%s: %+v, %v; want the head and the ebook hashed", rel, got, err)
		}
	}
	if got, _ := cat.BookReadalongAt(ctx, lib.ID, "B/Book"); got.Head != nil || got.EbookSHA256 != "" {
		t.Errorf("no map: %+v; want the ebook never hashed", got)
	}
	if got, _ := cat.BookReadalongAt(ctx, lib.ID, "Bad.m4b"); got.Head != nil || got.EbookSHA256 != "" {
		t.Errorf("a map that fails its checks: %+v; want none", got)
	}
	var sig string
	if err := db.QueryRowContext(ctx, `SELECT attachments_sig FROM books WHERE rel_path = 'Bad.m4b'`).Scan(&sig); err != nil ||
		!slices.Equal(metadata.AttachmentSigNames(sig), []string{"readalong.json"}) {
		t.Errorf("a refused map's signature = %q, %v", sig, err)
	}
	before := indexedAt(t, db, lib.ID, "A/Book")
	edited := append(slices.Clone(epub), 0)
	writeFile(t, root, "A/Book/Book.epub", edited)
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got, _ := cat.BookReadalongAt(ctx, lib.ID, "A/Book"); !got.DigestCurrent || got.EbookSHA256 != digestOf(edited) {
		t.Errorf("after the ebook changed: %+v; want it hashed again", got)
	}
	if after := indexedAt(t, db, lib.ID, "A/Book"); after != before {
		t.Errorf("the book was re-indexed: %q -> %q", before, after)
	}
}

// TestRefreshAttachmentsHashesReadalong: the Bridge's attachments rescan of a
// map it just wrote stores the head and hashes the ebook, so the map reads
// ready at once.
func TestRefreshAttachmentsHashesReadalong(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", "01.m4b"))
	writeFile(t, root, "A/Book/Book.epub", epub)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "A/Book/.audiosilo/readalong.json", testReadalong(digestOf(epub)), time.Now())
	got, err := s.RefreshAttachments(ctx, *lib, "A/Book")
	if err != nil || got.Attachments != (AttachmentsHeld{Readalong: true}) {
		t.Fatalf("refresh = %+v, %v", got, err)
	}
	if ra, _ := cat.BookReadalongAt(ctx, lib.ID, "A/Book"); ra.Head == nil || !ra.DigestCurrent || ra.EbookSHA256 != digestOf(epub) {
		t.Errorf("after the refresh: %+v", ra)
	}
}

// TestReadReadalongMap: the map is read from where the Bridge writes it, a
// regular file in a real, unignored .audiosilo folder; anything else is
// ErrNoReadalong.
func TestReadReadalongMap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := testReadalong(strings.Repeat("a", 64))
	writeFile(t, root, "A/Book/.audiosilo/readalong.json", []byte(body))
	writeFile(t, root, ".audiosilo/Loose.readalong.json", []byte(body))
	lib := catalog.Library{Root: root}
	for _, c := range []struct {
		path   string
		folder bool
	}{{"A/Book", true}, {"Loose.m4b", false}} {
		data, info, err := ReadReadalongMap(lib, c.path, c.folder)
		if err != nil || string(data) != body || info.Size() != int64(len(body)) {
			t.Errorf("%s: %d bytes, %v", c.path, len(data), err)
		}
	}
	// A linked .audiosilo folder, a linked map, an ignored folder, none.
	writeFile(t, root, "Elsewhere/readalong.json", []byte(body))
	if err := os.MkdirAll(filepath.Join(root, "C", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Elsewhere"), filepath.Join(root, "C", "Book", ".audiosilo")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "D/Book/.audiosilo/x.json", []byte(body))
	if err := os.Symlink(filepath.Join(root, "Elsewhere", "readalong.json"), filepath.Join(root, "D", "Book", ".audiosilo", "readalong.json")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		lib  catalog.Library
		path string
	}{
		"a linked folder": {lib, "C/Book"},
		"a linked map":    {lib, "D/Book"},
		"ignored":         {catalog.Library{Root: root, IgnorePatterns: []string{".audiosilo"}}, "A/Book"},
		"none":            {lib, "E/Book"},
		"outside":         {lib, "../x"},
	} {
		if _, _, err := ReadReadalongMap(c.lib, c.path, true); !errors.Is(err, ErrNoReadalong) {
			t.Errorf("%s: %v, want ErrNoReadalong", name, err)
		}
	}
}

// TestEbookDigest: the digest of the very file opened, with its signature as
// the scan records it.
func TestEbookDigest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	writeFile(t, root, "A/b.epub", epub)
	sha, sig, err := EbookDigest(context.Background(), root, "A/b.epub", true)
	info, _ := os.Stat(filepath.Join(root, "A", "b.epub"))
	if err != nil || sha != digestOf(epub) || sig != catalog.EbookSig("b.epub", info) {
		t.Errorf("digest = %q %q %v", sha, sig, err)
	}
	if _, _, err := EbookDigest(context.Background(), root, "A/none.epub", true); err == nil {
		t.Error("no file: no error")
	}
	// A request that has given up (its deadline, a cancelled scan) stops the hash.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := EbookDigest(done, root, "A/b.epub", true); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled", err)
	}
}
