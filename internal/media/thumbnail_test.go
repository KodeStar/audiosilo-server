package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
)

func encodePNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodedSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Fatalf("thumbnail format = %s, want jpeg", format)
	}
	return cfg.Width, cfg.Height
}

func TestThumbnailScalesDownKeepingAspect(t *testing.T) {
	t.Parallel()
	out, err := Thumbnail(encodePNG(t, 1200, 900, color.NRGBA{200, 30, 90, 255}), 320)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 320 || h != 240 {
		t.Fatalf("size = %dx%d, want 320x240", w, h)
	}
}

// noisyJPEG is a w x h JPEG of busy art (a gradient with fine detail) saved at
// quality q, as a cover.jpg from the web often is.
func noisyJPEG(t *testing.T, w, h, q int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8((x*y + y*7) % 256), uint8((x ^ y) * 3), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A JPEG already within the size is never scaled up: it keeps its own size, and
// comes back as the smaller of its own bytes and a re-encode at
// thumbnailQuality. A small JPEG Go can't decode fully is served as it is.
func TestThumbnailSmallJPEGNeverUpscaledSmallestBytes(t *testing.T) {
	t.Parallel()
	high := noisyJPEG(t, 180, 180, 95)
	out, err := Thumbnail(high, 320)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 180 || h != 180 {
		t.Fatalf("size = %dx%d, want the source's 180x180", w, h)
	}
	if len(out) >= len(high) {
		t.Fatalf("a quality-95 JPEG came back %d bytes, want fewer than its %d", len(out), len(high))
	}

	low := noisyJPEG(t, 200, 200, 40)
	if out, err := Thumbnail(low, 320); err != nil || !bytes.Equal(out, low) {
		t.Fatalf("a JPEG smaller than its re-encode should be returned unchanged (err %v)", err)
	}

	truncated := high[:len(high)/2]
	if out, err := Thumbnail(truncated, 320); err != nil || !bytes.Equal(out, truncated) {
		t.Fatalf("a small JPEG that doesn't decode fully should be returned unchanged (err %v)", err)
	}
}

// ThumbnailJPEG never hands back the source's bytes: a small JPEG carrying
// trailing bytes (a polyglot) comes back as this server's own encoding.
func TestThumbnailJPEGAlwaysReencodes(t *testing.T) {
	t.Parallel()
	src := append(encodeJPEG(t, 200, 200), []byte("<html>trailing</html>")...)
	out, err := ThumbnailJPEG(src, 320)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out, src) || bytes.Contains(out, []byte("<html>")) {
		t.Fatal("ThumbnailJPEG returned the source bytes")
	}
	if w, h := decodedSize(t, out); w != 200 || h != 200 {
		t.Fatalf("size = %dx%d", w, h)
	}
	if _, err := ThumbnailJPEG([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), 320); err == nil {
		t.Fatal("an SVG decoded")
	}
}

func TestThumbnailSmallPNGBecomesJPEGWithoutUpscaling(t *testing.T) {
	t.Parallel()
	out, err := Thumbnail(encodePNG(t, 100, 150, color.NRGBA{0, 0, 0, 0}), 320)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 100 || h != 150 {
		t.Fatalf("size = %dx%d, want 100x150 (never upscaled)", w, h)
	}
	// Transparency is flattened onto white, not black.
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := img.At(50, 75).RGBA(); r < 0xf000 {
		t.Fatalf("transparent pixel came out dark (r=%#x), want white", r)
	}
}

// A small file declaring enormous dimensions must be refused from its header,
// never decoded (a decompression bomb would allocate gigabytes).
func TestThumbnailRefusesDecompressionBomb(t *testing.T) {
	t.Parallel()
	src := encodePNG(t, 1, 1, color.White)
	// Rewrite the IHDR width/height (bytes 16-23 of a PNG) to 100000 x 100000 and
	// fix the chunk's CRC (over its type and data, bytes 12-28).
	bomb := append([]byte(nil), src...)
	copy(bomb[16:24], []byte{0, 1, 0x86, 0xa0, 0, 1, 0x86, 0xa0})
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	if _, err := Thumbnail(bomb, 320); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want ErrImageTooLarge", err)
	}
}

func TestThumbnailRejectsNonImage(t *testing.T) {
	t.Parallel()
	if _, err := Thumbnail([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), 320); err == nil {
		t.Fatal("an SVG (not a raster cover) should be refused")
	}
}

func TestReadLimited(t *testing.T) {
	t.Parallel()
	if got, err := ReadLimited(strings.NewReader("abc"), 3); err != nil || string(got) != "abc" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ReadLimited(strings.NewReader("abcd"), 3); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want ErrImageTooLarge", err)
	}
}

func TestFitWithin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ w, h, size, ww, wh int }{
		{1000, 1000, 320, 320, 320},
		{100, 50, 320, 100, 50},
		{4000, 10, 320, 320, 1},
		{10, 4000, 320, 1, 320},
	} {
		if w, h := fitWithin(c.w, c.h, c.size); w != c.ww || h != c.wh {
			t.Errorf("fitWithin(%d,%d,%d) = %d,%d, want %d,%d", c.w, c.h, c.size, w, h, c.ww, c.wh)
		}
	}
}

// Every type coverMIME accepts as art must decode: a BMP cover (old ID3 tags)
// would otherwise have has_cover set yet never a thumbnail.
func TestThumbnailDecodesBMP(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, ok := coverMIME(buf.Bytes()); !ok {
		t.Fatal("coverMIME refused a BMP")
	}
	out, err := Thumbnail(buf.Bytes(), 160)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 160 || h != 80 {
		t.Fatalf("size = %dx%d, want 160x80", w, h)
	}
}
