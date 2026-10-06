package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

func testImage(t *testing.T, w, h int, asPNG bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	var err error
	if asPNG {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedCovers indexes four books in a fresh library: one with a sidecar image, one
// that gets a custom cover, one with no art at all, and one whose recorded sidecar
// path escapes the library root.
func seedCovers(t *testing.T, e *testEnv) (libID int64, root string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	for _, dir := range []string{"Sidecar", "Custom", "Bare", "Escape"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		// An "audio" file with no tags: no embedded art.
		if err := os.WriteFile(filepath.Join(root, dir, "book.m4b"), []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Sidecar", "cover.jpg"), testImage(t, 800, 800, false), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []*catalog.Book{
		{RelPath: "Sidecar", CoverPath: "Sidecar/cover.jpg"},
		{RelPath: "Custom"},
		{RelPath: "Bare"},
		{RelPath: "Escape", CoverPath: "../../../../etc/passwd"},
	} {
		b.LibraryID, b.IsFolder, b.Title, b.Format = lib.ID, true, b.RelPath, "m4b"
		b.Files = []catalog.BookFile{{RelPath: b.RelPath + "/book.m4b", Seq: 1}}
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cat.SetCover(ctx, lib.ID, "Custom", testImage(t, 1000, 500, true), e.adminID); err != nil {
		t.Fatal(err)
	}
	return lib.ID, root
}

func coversBody(libID int64, size int, paths ...string) string {
	refs := make([]string, len(paths))
	for i, p := range paths {
		refs[i] = `{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":` + strconv.Quote(p) + `}`
	}
	body := `{"books":[` + strings.Join(refs, ",") + `]`
	if size != 0 {
		body += `,"size":` + strconv.Itoa(size)
	}
	return body + "}"
}

func postCovers(t *testing.T, e *testEnv, tok, body string) []coverThumb {
	t.Helper()
	resp, out := e.do(t, "POST", "/api/v1/admin/covers", tok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("covers = %d %s", resp.StatusCode, out)
	}
	var got struct{ Covers []coverThumb }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	return got.Covers
}

// thumbSize decodes a returned data: URL and reports its pixel size.
func thumbSize(t *testing.T, dataURL string) (int, int) {
	t.Helper()
	raw, ok := strings.CutPrefix(dataURL, "data:image/jpeg;base64,")
	if !ok {
		t.Fatalf("not a JPEG data URL: %.40q", dataURL)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestAdminCoversRequireAdmin(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	body := coversBody(libID, 0, "Sidecar")
	if resp, _ := e.do(t, "POST", "/api/v1/admin/covers", "", body); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", resp.StatusCode)
	}
	if resp, out := e.do(t, "POST", "/api/v1/admin/covers", memberTok, body); resp.StatusCode != http.StatusForbidden || strings.Contains(out, "data:") {
		t.Fatalf("member = %d %s, want 403 with no art", resp.StatusCode, out)
	}
	if got := postCovers(t, e, adminTok, body); got[0].Data == "" {
		t.Fatal("admin got no thumbnail")
	}
}

// TestAdminCovers: thumbnails come back in request order, scaled to the size, for
// sidecar and custom art; a book without art, an unknown library and a path no book
// is at are empty entries, not errors.
func TestAdminCovers(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	got := postCovers(t, e, adminTok, coversBody(libID, 160, "Sidecar", "Custom", "Bare", "Nope"))
	if len(got) != 4 || got[0].Path != "Sidecar" || got[3].Path != "Nope" || got[0].LibraryID != libID {
		t.Fatalf("covers = %+v", got)
	}
	if w, h := thumbSize(t, got[0].Data); w != 160 || h != 160 {
		t.Fatalf("sidecar thumb = %dx%d, want 160x160", w, h)
	}
	if w, h := thumbSize(t, got[1].Data); w != 160 || h != 80 {
		t.Fatalf("custom thumb = %dx%d, want 160x80 (aspect kept)", w, h)
	}
	if got[2].Data != "" || got[3].Data != "" {
		t.Fatalf("bare/missing = %q %q, want empty", got[2].Data, got[3].Data)
	}
	other := postCovers(t, e, adminTok, coversBody(libID+99, 0, "Sidecar"))
	if other[0].Data != "" {
		t.Fatal("an unknown library returned art")
	}
	// The default size is 320.
	if w, _ := thumbSize(t, postCovers(t, e, adminTok, coversBody(libID, 0, "Sidecar"))[0].Data); w != 320 {
		t.Fatalf("default thumb width = %d, want 320", w)
	}
}

// A sidecar path outside the library root (a crafted or stale record) is never
// read: SafeJoin refuses it, and the book reads as having no art.
func TestAdminCoversRefuseEscapingSidecar(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	if got := postCovers(t, e, adminTok, coversBody(libID, 0, "Escape", "../../etc")); got[0].Data != "" || got[1].Data != "" {
		t.Fatalf("escaping paths returned art: %+v", got)
	}
}

// A replaced cover is a new cache key: the next request sees the new art, not the
// cached thumbnail of the old one.
func TestAdminCoversFollowChangedArt(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, root := seedCovers(t, e)
	body := coversBody(libID, 0, "Sidecar", "Custom")
	first := postCovers(t, e, adminTok, body)

	sidecar := filepath.Join(root, "Sidecar", "cover.jpg")
	if err := os.WriteFile(sidecar, testImage(t, 400, 200, false), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(sidecar, later, later); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.SetCover(context.Background(), libID, "Custom", testImage(t, 200, 400, true), e.adminID); err != nil {
		t.Fatal(err)
	}
	second := postCovers(t, e, adminTok, body)
	if w, h := thumbSize(t, second[0].Data); w != 320 || h != 160 {
		t.Fatalf("changed sidecar thumb = %dx%d, want 320x160", w, h)
	}
	if second[1].Data == first[1].Data {
		t.Fatal("replaced custom cover still served the old thumbnail")
	}
	if w, h := thumbSize(t, second[1].Data); w != 160 || h != 320 {
		t.Fatalf("new custom thumb = %dx%d, want 160x320", w, h)
	}
}

func TestAdminCoversValidation(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	many := make([]string, maxCoverBatch+1)
	for i := range many {
		many[i] = "Sidecar"
	}
	for name, tc := range map[string]struct {
		body, code string
	}{
		"empty":    {`{"books":[]}`, ""},
		"too many": {coversBody(libID, 0, many...), codeTooLarge},
		"bad size": {coversBody(libID, 999, "Sidecar"), ""},
		"not json": {`nope`, ""},
	} {
		resp, out := e.do(t, "POST", "/api/v1/admin/covers", adminTok, tc.body)
		if resp.StatusCode != http.StatusBadRequest || (tc.code != "" && !strings.Contains(out, `"code":"`+tc.code+`"`)) {
			t.Errorf("%s: %d %s, want 400 %s", name, resp.StatusCode, out, tc.code)
		}
	}
}

// A sidecar over maxSidecarBytes is no art, and that is remembered: it stays
// oversized, so it must not be re-read on every page of covers.
func TestAdminCoversOversizedSidecarCached(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, root := seedCovers(t, e)
	sidecar := filepath.Join(root, "Sidecar", "cover.jpg")
	if err := os.Truncate(sidecar, maxSidecarBytes+1); err != nil {
		t.Fatal(err)
	}
	if got := postCovers(t, e, adminTok, coversBody(libID, 0, "Sidecar")); got[0].Data != "" {
		t.Fatal("an oversized sidecar returned art")
	}
	fi, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(libID, 10) + "\x00Sidecar\x00" + strconv.Itoa(defaultThumbSize) +
		"\x00" + fmt.Sprintf("s%d-%d", fi.Size(), fi.ModTime().UnixNano())
	if v, ok := e.api.thumbs.Get(key); !ok || v != nil {
		t.Fatalf("oversized sidecar cache entry = %d bytes, %v; want a cached no-art entry", len(v), ok)
	}
}
