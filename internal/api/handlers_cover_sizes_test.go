package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// media ?token= too) and the out-of-scope 403 outside it, with no art.
func TestCoverSizeScope(t *testing.T) {
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
}

// TestCoverSizeETag: a matching If-None-Match is a 304 with the same validator
// and cache headers; each size and each art version has its own ETag.
func TestCoverSizeETag(t *testing.T) {
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
	if err := e.cat.SetCover(context.Background(), libID, "Custom", bandedPNG(t, 300, 300), e.adminID); err != nil {
		t.Fatal(err)
	}
	resp, _ = e.doHeaders(t, "GET", coverURL(libID, "Custom", "320"), adminTok, "", map[string]string{"If-None-Match": old})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == old {
		t.Fatalf("replaced cover revalidation = %d (ETag %q), want 200 with a new ETag", resp.StatusCode, resp.Header.Get("ETag"))
	}
}

// TestCoverColorAndVersion: a custom upload moves the book's cover_version at
// once; a thumbnail (GET ?size= or the console's batch) records its colours,
// seen on the item and in lists; removing the custom cover clears both.
func TestCoverColorAndVersion(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, root := seedCovers(t, e)
	id := strconv.FormatInt(libID, 10)

	// Seeded with a custom cover: a version already, no colours until a thumbnail.
	cc, seeded := itemCover(t, e, adminTok, libID, "Custom")
	if seeded == "" || cc != nil {
		t.Fatalf("seeded custom cover: color %+v version %q, want a version and no colour", cc, seeded)
	}

	resp, body := e.do(t, "PUT", "/api/v1/admin/libraries/"+id+"/cover?path=Custom", adminTok, string(bandedPNG(t, 400, 400)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d %s", resp.StatusCode, body)
	}
	cc, uploaded := itemCover(t, e, adminTok, libID, "Custom")
	if uploaded == "" || uploaded == seeded || cc != nil {
		t.Fatalf("after upload: color %+v version %q (was %q), want a new version and no colour", cc, uploaded, seeded)
	}

	if resp, _ := e.do(t, "GET", coverURL(libID, "Custom", "320"), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb = %d", resp.StatusCode)
	}
	cc, version := itemCover(t, e, adminTok, libID, "Custom")
	if version != uploaded || cc == nil || cc.Bg != "#141e50" || cc.Accent == "" || cc.OnAccent == "" {
		t.Fatalf("after thumbnail: color %+v version %q, want navy bg with an accent at %q", cc, version, uploaded)
	}
	_, body = e.do(t, "GET", "/api/v1/libraries/"+id+"/books", adminTok, "")
	if !strings.Contains(body, `"cover_color":{"bg":"#141e50"`) || !strings.Contains(body, `"cover_version":"`+uploaded+`"`) {
		t.Fatalf("list lacks the custom cover's colour/version: %s", body)
	}

	// The console's batch records too: the sidecar's version is its file's.
	postCovers(t, e, adminTok, coversBody(libID, 160, "Sidecar"))
	fi, err := os.Stat(filepath.Join(root, "Sidecar", "cover.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	cc, version = itemCover(t, e, adminTok, libID, "Sidecar")
	want := catalog.CoverVersion(fmt.Sprintf("s%d-%d", fi.Size(), fi.ModTime().UnixNano()))
	if version != want || cc == nil || cc.Bg != "#000000" || cc.Accent != "" {
		t.Fatalf("sidecar after batch: color %+v version %q, want black bg, no accent, %q", cc, version, want)
	}

	// A re-index that touches the book clears them; the next thumbnail restores
	// them even from the cache (the art itself did not change).
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: libID, RelPath: "Sidecar",
		IsFolder: true, Title: "Sidecar", Format: "m4b", CoverPath: "Sidecar/cover.jpg", MTime: 42,
		Files: []catalog.BookFile{{RelPath: "Sidecar/book.m4b", Seq: 1}}}); err != nil {
		t.Fatal(err)
	}
	if cc, version := itemCover(t, e, adminTok, libID, "Sidecar"); cc != nil || version != "" {
		t.Fatalf("after re-index: color %+v version %q, want both cleared", cc, version)
	}
	if resp, _ := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("cached thumb = %d", resp.StatusCode)
	}
	if cc, version := itemCover(t, e, adminTok, libID, "Sidecar"); cc == nil || version != want {
		t.Fatalf("after a cached thumbnail: color %+v version %q, want restored %q", cc, version, want)
	}

	resp, body = e.do(t, "DELETE", "/api/v1/admin/libraries/"+id+"/cover?path=Custom", adminTok, "")
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if cc, version := itemCover(t, e, adminTok, libID, "Custom"); cc != nil || version != "" {
		t.Fatalf("after delete: color %+v version %q, want both cleared", cc, version)
	}
}

func TestServerInfoCoverSizes(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"cover_sizes":true`) {
		t.Fatalf("server info lacks cover_sizes: %s", body)
	}
}
