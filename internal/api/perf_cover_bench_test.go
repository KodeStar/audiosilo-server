package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// coverJPEG is a 180x180 JPEG, the size of a LibriVox cover.jpg.
func coverJPEG(tb testing.TB) []byte {
	tb.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 180, 180))
	for y := range 180 {
		for x := range 180 {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkPerfCover times the harness's cached grid cover
// (GET /libraries/{id}/cover?path=&size=320&v=&token=, an image Accept, no
// conditional headers) in-process: media auth, the cover's sources, the art's
// version and the thumbnail cache.
//
//	go test ./internal/api -run '^$' -bench BenchmarkPerfCover -benchtime 2s
func BenchmarkPerfCover(b *testing.B) {
	e := seedPerfLibrary(b, perfBooks())
	path := ""
	for _, p := range e.books {
		if filepath.Ext(p) == "" { // a folder book with cover.jpg
			path = p
			break
		}
	}
	abs := filepath.Join(e.root, filepath.FromSlash(path))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "cover.jpg"), coverJPEG(b), 0o644); err != nil {
		b.Fatal(err)
	}
	u := "/api/v1/libraries/" + strconv.FormatInt(e.libID, 10) + "/cover?path=" + url.QueryEscape(path) +
		"&size=320&v=x&token=" + url.QueryEscape(e.token)
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.RemoteAddr = "192.0.2.1:4000"
		req.Header.Set(auth.ClientHeader, "AudioSilo/2.4.1 (web)")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" {
		b.Fatalf("first cover: %d %s", rec.Code, rec.Body.String())
	}
	b.ReportAllocs()
	for b.Loop() {
		if rec := get(); rec.Code != http.StatusOK {
			b.Fatalf("cover: %d", rec.Code)
		}
	}
}
