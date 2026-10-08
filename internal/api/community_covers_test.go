package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// loopbackFetch stands in for meta.Service.FetchCover with loopback allowed, so
// the test's cover host can answer; the address guard itself is meta's to test,
// and each test first shows the real fetcher refusing loopback.
func loopbackFetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, meta.ErrCoverURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, meta.ErrCoverURL
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cover answered %d", resp.StatusCode)
	}
	return media.ReadLimited(resp.Body, limit)
}

// coverHost serves community cover images: a small JPEG, a PNG larger than an
// upload may be, a page that isn't an image, a decompression bomb, and a 404.
func coverHost(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	var small bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 60))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	if err := jpeg.Encode(&small, img, nil); err != nil {
		t.Fatal(err)
	}
	// Noise doesn't compress: 1800 x 1800 is about 13 MB as a PNG.
	big := image.NewRGBA(image.Rect(0, 0, 1800, 1800))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range big.Pix {
		if i%4 == 3 {
			big.Pix[i] = 255 // opaque
		} else {
			big.Pix[i] = byte(r.Uint32())
		}
	}
	var bigPNG bytes.Buffer
	if err := png.Encode(&bigPNG, big); err != nil {
		t.Fatal(err)
	}
	if bigPNG.Len() <= catalog.MaxCoverBytes || bigPNG.Len() > maxCommunityCoverBytes {
		t.Fatalf("big cover is %d bytes, want between the upload cap and the fetch cap", bigPNG.Len())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/small.jpg", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(small.Bytes()) })
	mux.HandleFunc("/big.png", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bigPNG.Bytes()) })
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>not a cover</body></html>"))
	})
	bomb := pngHeader(20000, 20000)
	mux.HandleFunc("/bomb.png", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bomb) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, small.Bytes()
}

// pngHeader is the start of a PNG claiming w x h pixels: its signature and IHDR,
// all image.DecodeConfig reads (a decompression bomb's few bytes).
func pngHeader(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA, no interlace
	chunk := append([]byte("IHDR"), ihdr...)
	out := append([]byte("\x89PNG\r\n\x1a\n"), binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)))...)
	out = append(out, chunk...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(chunk))
}

func TestCommunityCovers(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, memberTok, _ := adminAndMember(t, e)
	host, _ := coverHost(t)
	const url = "/api/v1/admin/meta/covers"
	batch := func(urls ...string) string {
		b, _ := json.Marshal(map[string]any{"urls": urls, "size": 160})
		return string(b)
	}
	thumbs := func(tok, body string) []string {
		t.Helper()
		resp, got := e.do(t, "POST", url, tok, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("covers = %d %s", resp.StatusCode, got)
		}
		var out struct{ Covers []string }
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatal(err)
		}
		return out.Covers
	}

	// Denied: the server's own network is never fetched, loopback included.
	if got := thumbs(adminTok, batch(host.URL+"/small.jpg")); len(got) != 1 || got[0] != "" {
		t.Fatalf("a loopback cover was fetched: %v", got)
	}

	// Allowed (loopback standing in for the internet): a thumbnail per cover, in
	// order, "" for what isn't one.
	e.api.fetchCover = loopbackFetch
	got := thumbs(adminTok, batch(host.URL+"/missing.jpg", host.URL+"/big.png", "ftp://x/c.jpg",
		host.URL+"/page.html", host.URL+"/small.jpg"))
	if len(got) != 5 || got[0] != "" || got[2] != "" || got[3] != "" ||
		!strings.HasPrefix(got[1], "data:image/jpeg;base64,") || !strings.HasPrefix(got[4], "data:image/jpeg;base64,") {
		t.Fatalf("covers = %.60q", got)
	}

	if resp, _ := e.do(t, "POST", url, memberTok, batch(host.URL+"/small.jpg")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member covers = %d, want 403", resp.StatusCode)
	}
	many := make([]string, maxCommunityCovers+1)
	for i := range many {
		many[i] = host.URL + "/small.jpg?" + strconv.Itoa(i)
	}
	for body, code := range map[string]int{
		`{"urls":[]}`:  http.StatusBadRequest,
		batch(many...): http.StatusBadRequest,
		`{"urls":["` + host.URL + `/small.jpg"],"size":100}`: http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if resp, got := e.do(t, "POST", url, adminTok, body); resp.StatusCode != code {
			t.Errorf("POST %.60s = %d %s, want %d", body, resp.StatusCode, got, code)
		}
	}
}

func TestSetCommunityCover(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, memberTok, _ := adminAndMember(t, e)
	host, small := coverHost(t)
	ctx := context.Background()
	libID := seedBook(t, e, "Andy Weir/The Martian", "")
	const p = "Andy Weir/The Martian"
	url := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/cover/community?path=" + escape(p)
	set := func(tok, path, cover string) (int, string) {
		t.Helper()
		resp, body := e.do(t, "PUT", path, tok, `{"url":"`+cover+`"}`)
		return resp.StatusCode, body
	}

	// Denied: a cover on the server's own network is not fetched.
	if code, body := set(adminTok, url, host.URL+"/small.jpg"); code != http.StatusBadGateway ||
		!strings.Contains(body, `"code":"cover_unavailable"`) {
		t.Fatalf("loopback cover = %d %s, want 502 cover_unavailable", code, body)
	}
	if _, err := e.cat.Cover(ctx, libID, p); err == nil {
		t.Fatal("a refused cover was stored")
	}

	e.api.fetchCover = loopbackFetch
	if code, _ := set(memberTok, url, host.URL+"/small.jpg"); code != http.StatusForbidden {
		t.Fatalf("member set = %d, want 403", code)
	}
	for _, tc := range []struct {
		path, cover string
		code        int
		errCode     string
	}{
		{url, "javascript:alert(1)", http.StatusBadRequest, ""},
		{url, host.URL + "/missing.jpg", http.StatusBadGateway, "cover_unavailable"},
		{url, host.URL + "/page.html", http.StatusUnsupportedMediaType, "unsupported_image"},
		// A few bytes claiming more pixels than a cover is ever decoded from.
		{url, host.URL + "/bomb.png", http.StatusRequestEntityTooLarge, "too_large"},
		{strings.Replace(url, escape(p), escape("Andy Weir/Artemis"), 1), host.URL + "/small.jpg", http.StatusNotFound, ""},
	} {
		if code, body := set(adminTok, tc.path, tc.cover); code != tc.code || !strings.Contains(body, tc.errCode) {
			t.Errorf("set %s = %d %s, want %d %s", tc.cover, code, body, tc.code, tc.errCode)
		}
	}

	// Allowed: kept as the book's custom cover, as fetched.
	if code, body := set(adminTok, url, host.URL+"/small.jpg"); code != http.StatusOK {
		t.Fatalf("set = %d %s", code, body)
	}
	if cv, err := e.cat.Cover(ctx, libID, p); err != nil || !bytes.Equal(cv.Data, small) {
		t.Fatalf("stored cover = %v, want the fetched image", err)
	}
	// Kept as a community cover: clearing the community matches takes it.
	if cleared, err := e.cat.ClearCommunityMatches(ctx, libID); err != nil || cleared.Covers != 1 {
		t.Fatalf("clear = %+v %v, want the community cover removed", cleared, err)
	}
	// One larger than an upload may be is kept re-encoded, within storedCoverSide.
	if code, body := set(adminTok, url, host.URL+"/big.png"); code != http.StatusOK {
		t.Fatalf("set big = %d %s", code, body)
	}
	cv, err := e.cat.Cover(ctx, libID, p)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(cv.Data))
	if err != nil || format != "jpeg" || cfg.Width != storedCoverSide || len(cv.Data) > catalog.MaxCoverBytes {
		t.Fatalf("big cover stored as %s %dx%d, %d bytes (%v)", format, cfg.Width, cfg.Height, len(cv.Data), err)
	}
}

// Metadata off stops every outbound call: neither endpoint fetches anything.
func TestCommunityCoversNeedMetadata(t *testing.T) {
	e := newMetaEnv(t, false, 0)
	adminTok, _, _ := adminAndMember(t, e)
	libID := seedBook(t, e, "Andy Weir/The Martian", "")
	for _, req := range []struct{ method, path, body string }{
		{"POST", "/api/v1/admin/meta/covers", `{"urls":["https://example.com/c.jpg"]}`},
		{"PUT", "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/cover/community?path=" +
			escape("Andy Weir/The Martian"), `{"url":"https://example.com/c.jpg"}`},
	} {
		if resp, body := e.do(t, req.method, req.path, adminTok, req.body); resp.StatusCode != http.StatusNotFound ||
			!strings.Contains(body, `"code":"metadata_off"`) {
			t.Errorf("%s %s = %d %s, want 404 metadata_off", req.method, req.path, resp.StatusCode, body)
		}
	}
}
