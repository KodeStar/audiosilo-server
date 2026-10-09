package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

func coverURL(libID int64, path string, size string) string {
	u := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/cover?path=" + url.QueryEscape(path)
	if size != "" {
		u += "&size=" + size
	}
	return u
}

// jpegSize decodes a JPEG response body and reports its pixel size.
func jpegSize(t *testing.T, body string) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(strings.NewReader(body))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return cfg.Width, cfg.Height
}

// bandedPNG is a w x h PNG, its top 70% navy and the rest orange: a cover with a
// dominant colour and a vibrant one that reads against it.
func bandedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		c := color.NRGBA{20, 30, 80, 255}
		if y >= h*7/10 {
			c = color.NRGBA{240, 140, 20, 255}
		}
		for x := range w {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// itemCover fetches GET /item for a path and returns its cover_color and
// cover_version.
func itemCover(t *testing.T, e *testEnv, tok string, libID int64, path string) (*catalog.CoverColor, string) {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/item?path="+url.QueryEscape(path), tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item %q = %d %s", path, resp.StatusCode, body)
	}
	var b struct {
		CoverColor   *catalog.CoverColor `json:"cover_color"`
		CoverVersion string              `json:"cover_version"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	return b.CoverColor, b.CoverVersion
}

// TestCoverSizeThumbnail: ?size= answers a JPEG within the size from the same art
// as the full cover (sidecar, custom, by a part path too), with cache headers by
// source; no art is a 404; no size is the full art, unchanged.
func TestCoverSizeThumbnail(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)

	resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), adminTok, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("sidecar thumb = %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if w, h := jpegSize(t, body); w != 160 || h != 160 {
		t.Fatalf("sidecar thumb = %dx%d, want 160x160", w, h)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=86400" || resp.Header.Get("ETag") == "" {
		t.Fatalf("sidecar thumb headers: Cache-Control %q, ETag %q", cc, resp.Header.Get("ETag"))
	}

	for _, p := range []string{"Custom", "Custom/book.m4b"} {
		resp, body = e.do(t, "GET", coverURL(libID, p, "640"), adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("custom thumb by %q = %d %s", p, resp.StatusCode, body)
		}
		if w, h := jpegSize(t, body); w != 640 || h != 320 {
			t.Fatalf("custom thumb by %q = %dx%d, want 640x320", p, w, h)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "private, no-cache" {
			t.Fatalf("custom thumb Cache-Control = %q, want private, no-cache", cc)
		}
	}

	for _, p := range []string{"Bare", "Escape", "Nope"} {
		resp, body = e.do(t, "GET", coverURL(libID, p, "320"), adminTok, "")
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Cache-Control") != "" {
			t.Fatalf("%s thumb = %d %s (Cache-Control %q), want an uncached 404", p, resp.StatusCode, body,
				resp.Header.Get("Cache-Control"))
		}
	}

	// No size: the full sidecar image, as before.
	resp, body = e.do(t, "GET", coverURL(libID, "Sidecar", ""), adminTok, "")
	if w, h := jpegSize(t, body); resp.StatusCode != http.StatusOK || w != 800 || h != 800 {
		t.Fatalf("full cover = %d %dx%d, want 200 800x800", resp.StatusCode, w, h)
	}
}

func TestCoverSizeValidation(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	for _, size := range []string{"999", "abc", "0", "-160", "160.0"} {
		resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", size), adminTok, "")
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"error"`) {
			t.Errorf("size=%s: %d %s, want 400 {error}", size, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", "")+"&size=", adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty size: %d %s, want 400", resp.StatusCode, body)
	}
}

// TestCoverSizeScope: a scoped user gets thumbnails inside the grant (by the
// media ?token= too) and the out-of-scope 403 outside it, with no art, including
// for a granted part path whose book lies outside the grant.
func TestCoverSizeScope(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	libID, _ := seedCovers(t, e)
	kid, err := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Sidecar only"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: libID, Path: "Sidecar"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, kid.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	tok, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)

	if resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("in-scope thumb = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "GET", coverURL(libID, "Sidecar", "160")+"&token="+url.QueryEscape(tok), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("in-scope thumb by ?token= = %d %s", resp.StatusCode, body)
	}
	resp, body := e.do(t, "GET", coverURL(libID, "Custom", "160"), tok, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, msgNoPathAccess) {
		t.Fatalf("out-of-scope thumb = %d %s, want 403 %q", resp.StatusCode, body, msgNoPathAccess)
	}
	if resp, _ := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous thumb = %d, want 401", resp.StatusCode)
	}

	// A grant of a part path only: its book lies above it, outside the grant.
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: libID, Path: "Custom/book.m4b"}); err != nil {
		t.Fatal(err)
	}
	resp, body = e.do(t, "GET", coverURL(libID, "Custom/book.m4b", "160"), tok, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, msgNoPathAccess) {
		t.Fatalf("thumb by a part path of an out-of-scope book = %d %s, want 403 %q", resp.StatusCode, body, msgNoPathAccess)
	}
}

// TestCoverSizeETag: a matching If-None-Match is a 304 with the same validator
// and cache headers; each size and each art version has its own ETag.
func TestCoverSizeETag(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	for _, p := range []string{"Sidecar", "Custom"} {
		resp, _ := e.do(t, "GET", coverURL(libID, p, "320"), adminTok, "")
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", p)
		}
		resp, body := e.doHeaders(t, "GET", coverURL(libID, p, "320"), adminTok, "", map[string]string{"If-None-Match": etag})
		if resp.StatusCode != http.StatusNotModified || body != "" || resp.Header.Get("ETag") != etag ||
			resp.Header.Get("Cache-Control") == "" {
			t.Fatalf("%s revalidation = %d %q (ETag %q, Cache-Control %q), want 304", p, resp.StatusCode, body,
				resp.Header.Get("ETag"), resp.Header.Get("Cache-Control"))
		}
		other, _ := e.do(t, "GET", coverURL(libID, p, "160"), adminTok, "")
		if other.Header.Get("ETag") == etag {
			t.Fatalf("%s: sizes 160 and 320 share an ETag", p)
		}
	}

	// A replaced custom cover no longer matches the old validator.
	resp, _ := e.do(t, "GET", coverURL(libID, "Custom", "320"), adminTok, "")
	old := resp.Header.Get("ETag")
	if err := e.cat.SetCover(context.Background(), libID, "Custom", bandedPNG(t, 300, 300), e.adminID, catalog.SourceEdited); err != nil {
		t.Fatal(err)
	}
	resp, _ = e.doHeaders(t, "GET", coverURL(libID, "Custom", "320"), adminTok, "", map[string]string{"If-None-Match": old})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == old {
		t.Fatalf("replaced cover revalidation = %d (ETag %q), want 200 with a new ETag", resp.StatusCode, resp.Header.Get("ETag"))
	}
}

// listCovers fetches GET /books and returns each book's cover_version and
// cover_color by path.
func listCovers(t *testing.T, e *testEnv, tok string, libID int64) map[string]struct {
	Color   *catalog.CoverColor
	Version string
} {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/books", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("books = %d %s", resp.StatusCode, body)
	}
	var page struct {
		Books []struct {
			Path         string              `json:"rel_path"`
			CoverColor   *catalog.CoverColor `json:"cover_color"`
			CoverVersion string              `json:"cover_version"`
		} `json:"books"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		Color   *catalog.CoverColor
		Version string
	}{}
	for _, b := range page.Books {
		out[b.Path] = struct {
			Color   *catalog.CoverColor
			Version string
		}{b.CoverColor, b.CoverVersion}
	}
	return out
}

// TestCoverColorAndVersion: every book carries a cover_version from its first
// list response, before any thumbnail; a custom upload moves it at once and a
// removal reverts it to the file art's; a thumbnail (GET ?size= or the console's
// batch) records the colour, which stops showing once the version moves on.
func TestCoverColorAndVersion(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	id := strconv.FormatInt(libID, 10)

	first := listCovers(t, e, adminTok, libID)
	for _, p := range []string{"Sidecar", "Custom", "Bare", "Escape"} {
		if b := first[p]; b.Version == "" || b.Color != nil {
			t.Fatalf("first list: %s has version %q colour %+v, want a version and no colour", p, b.Version, b.Color)
		}
	}
	fileVersion := first["Bare"].Version

	upload := func() string {
		t.Helper()
		resp, body := e.do(t, "PUT", "/api/v1/admin/libraries/"+id+"/cover?path=Bare", adminTok, string(bandedPNG(t, 400, 400)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload = %d %s", resp.StatusCode, body)
		}
		cc, v := itemCover(t, e, adminTok, libID, "Bare")
		if v == "" || v == fileVersion || cc != nil {
			t.Fatalf("after upload: color %+v version %q (file art %q), want a new version and no colour", cc, v, fileVersion)
		}
		return v
	}
	uploaded := upload()

	if resp, _ := e.do(t, "GET", coverURL(libID, "Bare", "320"), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb = %d", resp.StatusCode)
	}
	cc, version := itemCover(t, e, adminTok, libID, "Bare")
	if version != uploaded || cc == nil || cc.Bg != "#141e50" || cc.Accent == "" || cc.OnAccent == "" {
		t.Fatalf("after thumbnail: color %+v version %q, want navy bg with an accent at %q", cc, version, uploaded)
	}
	if b := listCovers(t, e, adminTok, libID)["Bare"]; b.Color == nil || *b.Color != *cc || b.Version != uploaded {
		t.Fatalf("list after thumbnail: %+v, want %+v at %q", b, cc, uploaded)
	}

	// The console's batch records too, and moves the version from the index's
	// identity to the art's own: the one the thumbnail's ETag carries.
	postCovers(t, e, adminTok, coversBody(libID, 160, "Sidecar"))
	cc, artVersion := itemCover(t, e, adminTok, libID, "Sidecar")
	thumb, _ := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), adminTok, "")
	etag := thumb.Header.Get("ETag")
	if artVersion == first["Sidecar"].Version || etag != `"thumb-160-`+artVersion+`"` || cc == nil || cc.Bg != "#000000" || cc.Accent != "" {
		t.Fatalf("sidecar after batch: color %+v version %q (ETag %s), want black bg, no accent, the art's version", cc, artVersion, etag)
	}

	// A re-index that touches the book moves its version, and its colour stops
	// showing; the next thumbnail records it again (from the cache: the art
	// itself did not change), even one that only revalidates a copy and is
	// answered 304, and the version is the art's own again.
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: libID, RelPath: "Sidecar",
		IsFolder: true, Title: "Sidecar", Format: "m4b", CoverPath: "Sidecar/cover.jpg", MTime: 42,
		Files: []catalog.BookFile{{RelPath: "Sidecar/book.m4b", Seq: 1}}}); err != nil {
		t.Fatal(err)
	}
	cc, touched := itemCover(t, e, adminTok, libID, "Sidecar")
	if cc != nil || touched == "" || touched == artVersion {
		t.Fatalf("after re-index: color %+v version %q, want a new version and no colour", cc, touched)
	}
	if resp, _ := e.doHeaders(t, "GET", coverURL(libID, "Sidecar", "160"), adminTok, "", map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidated thumb = %d, want 304", resp.StatusCode)
	}
	if cc, version := itemCover(t, e, adminTok, libID, "Sidecar"); cc == nil || version != artVersion {
		t.Fatalf("after a revalidated thumbnail: color %+v version %q, want a colour at %q", cc, version, artVersion)
	}

	// Another upload: the colour read from the previous one no longer shows.
	upload()

	resp, body := e.do(t, "DELETE", "/api/v1/admin/libraries/"+id+"/cover?path=Bare", adminTok, "")
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if cc, version := itemCover(t, e, adminTok, libID, "Bare"); cc != nil || version != fileVersion {
		t.Fatalf("after delete: color %+v version %q, want no colour and the file art's %q", cc, version, fileVersion)
	}
}

// TestCoverSidecarReplacedInPlace: a sidecar image overwritten in place leaves
// the index as it was (the audio's mtime and size, the sidecar's path), so no
// re-index moves the book's cover; its next thumbnail does - a new ETag, the new
// art's colour, and a cover_version that moves with them.
func TestCoverSidecarReplacedInPlace(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, root := seedCovers(t, e)

	first, _ := e.do(t, "GET", coverURL(libID, "Sidecar", "320"), adminTok, "")
	oldColor, oldVersion := itemCover(t, e, adminTok, libID, "Sidecar")
	if first.StatusCode != http.StatusOK || oldColor == nil || oldColor.Bg != "#000000" {
		t.Fatalf("first thumbnail = %d, colour %+v", first.StatusCode, oldColor)
	}

	sidecar := filepath.Join(root, "Sidecar", "cover.jpg")
	if err := os.WriteFile(sidecar, bandedPNG(t, 300, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(sidecar, later, later); err != nil {
		t.Fatal(err)
	}

	next, _ := e.doHeaders(t, "GET", coverURL(libID, "Sidecar", "320"), adminTok, "", map[string]string{"If-None-Match": first.Header.Get("ETag")})
	cc, version := itemCover(t, e, adminTok, libID, "Sidecar")
	if next.StatusCode != http.StatusOK || next.Header.Get("ETag") == first.Header.Get("ETag") {
		t.Fatalf("replaced art = %d (ETag %s), want 200 with a new ETag", next.StatusCode, next.Header.Get("ETag"))
	}
	if cc == nil || cc.Bg != "#141e50" || version == oldVersion || next.Header.Get("ETag") != `"thumb-320-`+version+`"` {
		t.Fatalf("after the replaced art's thumbnail: colour %+v version %q (was %q, ETag %s), want navy at the new art's version",
			cc, version, oldVersion, next.Header.Get("ETag"))
	}
}

// id3WithCover is an .mp3 that is nothing but an ID3v2.3 tag holding one APIC
// (front cover) frame with the JPEG img: a book with embedded art.
func id3WithCover(img []byte) []byte {
	var frame bytes.Buffer
	frame.WriteByte(0)                  // text encoding: ISO-8859-1
	frame.WriteString("image/jpeg\x00") // MIME type
	frame.WriteByte(3)                  // picture type: front cover
	frame.WriteByte(0)                  // an empty description
	frame.Write(img)
	var tag bytes.Buffer
	tag.WriteString("APIC")
	_ = binary.Write(&tag, binary.BigEndian, uint32(frame.Len()))
	tag.Write([]byte{0, 0}) // frame flags
	tag.Write(frame.Bytes())
	n := tag.Len() // the header's size is syncsafe: 7 bits a byte
	head := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(head, tag.Bytes()...)
}

// TestCoverEmbeddedReadFailureRetried: embedded art that can't be read right now
// (here an audio file that won't open, as on a mount in trouble) is a 500 the
// next request retries, not "no art" cached for that art's version.
func TestCoverEmbeddedReadFailureRetried(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	root := t.TempDir()
	audio := filepath.Join(root, "Emb", "book.mp3")
	if err := os.MkdirAll(filepath.Dir(audio), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, id3WithCover(testImage(t, 400, 400, false)), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: "Emb", IsFolder: true, Title: "Emb",
		Format: "mp3", Files: []catalog.BookFile{{RelPath: "Emb/book.mp3", Seq: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(audio, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(audio, 0o644) })

	if resp, body := e.do(t, "GET", coverURL(lib.ID, "Emb", "160"), adminTok, ""); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unreadable embedded art = %d %s, want 500", resp.StatusCode, body)
	}
	if err := os.Chmod(audio, 0o644); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "GET", coverURL(lib.ID, "Emb", "160"), adminTok, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("readable again = %d %s, want the thumbnail", resp.StatusCode, body)
	}
}

func TestServerInfoCoverSizes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"cover_sizes":true`) {
		t.Fatalf("server info lacks cover_sizes: %s", body)
	}
}
