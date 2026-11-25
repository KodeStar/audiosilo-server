package media

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
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

// jpegWith is jpg with segs (whole marker segments) put right after its start
// of image, where an editor writes its EXIF and colour profile.
func jpegWith(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

// segment is a JPEG marker segment holding payload.
func segment(marker byte, payload []byte) []byte {
	s := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:], uint16(len(payload)+2))
	return append(s, payload...)
}

// exifSegment is an APP1 EXIF segment whose first IFD holds an Orientation, in
// byte order "II" or "MM".
func exifSegment(order string, orientation uint16) []byte {
	var bo binary.ByteOrder = binary.BigEndian
	if order == "II" {
		bo = binary.LittleEndian
	}
	tiff := make([]byte, 26)
	copy(tiff, order)
	bo.PutUint16(tiff[2:], 42)
	bo.PutUint32(tiff[4:], 8) // the first IFD
	bo.PutUint16(tiff[8:], 1) // one entry
	bo.PutUint16(tiff[10:], 0x0112)
	bo.PutUint16(tiff[12:], 3) // SHORT
	bo.PutUint32(tiff[14:], 1)
	bo.PutUint16(tiff[18:], orientation)
	return segment(0xE1, append([]byte("Exif\x00\x00"), tiff...))
}

// cmykJPEG is an 8 x 8 CMYK JPEG (Adobe APP14, no colour profile).
const cmykJPEG = "/9j/4AAQSkZJRgABAQAASABIAAD/7QA4UGhvdG9zaG9wIDMuMAA4QklNBAQAAAAAAAA4QklNBCUAAAAAABDUHYzZjwCyBOmACZjs" +
	"+EJ+/+4ADkFkb2JlAGQAAAAAAP/AABQIAAgACAQBEQACEQEDEQEEEQH/xAAfAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgv/" +
	"xAC1EAACAQMDAgQDBQUEBAAAAX0BAgMABBEFEiExQQYTUWEHInEUMoGRoQgjQrHBFVLR8CQzYnKCCQoWFxgZGiUmJygpKjQ1Njc4" +
	"OTpDREVGR0hJSlNUVVZXWFlaY2RlZmdoaWpzdHV2d3h5eoOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbH" +
	"yMnK0tPU1dbX2Nna4eLj5OXm5+jp6vHy8/T19vf4+fr/xAAfAQADAQEBAQEBAQEBAAAAAAAAAQIDBAUGBwgJCgv/xAC1EQACAQIE" +
	"BAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdI" +
	"SUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV" +
	"1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2wBDAAICAgICAgMCAgMFAwMDBQYFBQUFBggGBgYGBggKCAgICAgICgoKCgoKCgoMDAwM" +
	"DAwODg4ODg8PDw8PDw8PDw//2wBDAQIDAwQEBAcEBAcQCwkLEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ" +
	"EBAQEBAQEBAQEBD/3QAEAAH/2gAOBAEAAhEDEQQRAD8A6D/gmC8Rm1CziyJ5CJJASNjAdDL2BTGR/e6Dua/f/FeWD9lSp0oJTVm3" +
	"ZLm0918v2XbZPpdI/UeHcmzPKcxjisTzLAJunRpQbaoJvlrUaNaOtWlUl+7qVKelLSrVUeVKP+wnCuDlhsQowXLRjpGKtaHRxi1o" +
	"10dtt30P/9k="

// A small JPEG drawn from more than its pixels (an EXIF orientation, a colour
// profile, CMYK) comes back as its own bytes, which a re-encode would lose or
// convert; one carrying only what changes nothing drawn is re-encoded.
func TestThumbnailSmallJPEGKeepsWhatAViewerApplies(t *testing.T) {
	t.Parallel()
	plain := noisyJPEG(t, 180, 180, 95)
	cmyk, err := base64.StdEncoding.DecodeString(cmykJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(cmyk)); err != nil || cfg.ColorModel != color.CMYKModel {
		t.Fatalf("the CMYK fixture decodes as %v (err %v)", cfg.ColorModel, err)
	}
	broken := exifSegment("II", 6)
	broken = broken[:len(broken)-14] // an IFD entry cut short
	binary.BigEndian.PutUint16(broken[2:], uint16(len(broken)-2))
	for _, c := range []struct {
		name string
		src  []byte
		keep bool
	}{
		{"orientation 6, little-endian", jpegWith(plain, exifSegment("II", 6)), true},
		{"orientation 8, big-endian", jpegWith(plain, exifSegment("MM", 8)), true},
		{"an ICC profile", jpegWith(plain, segment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), make([]byte, 600)...))), true},
		{"EXIF that can't be read", jpegWith(plain, broken), true},
		{"CMYK", cmyk, true},
		{"orientation 1", jpegWith(plain, exifSegment("II", 1)), false},
		{"XMP only", jpegWith(plain, segment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>"))), false},
	} {
		out, err := Thumbnail(c.src, 320)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if kept := bytes.Equal(out, c.src); kept != c.keep {
			t.Errorf("%s: source kept = %v (%d bytes from %d), want %v", c.name, kept, len(out), len(c.src), c.keep)
		}
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

// Every type metadata.CoverMIME accepts as art must decode: a BMP cover (old ID3 tags)
// would otherwise have has_cover set yet never a thumbnail.
func TestThumbnailDecodesBMP(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.CoverMIME(buf.Bytes()); !ok {
		t.Fatal("metadata.CoverMIME refused a BMP")
	}
	out, err := Thumbnail(buf.Bytes(), 160)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 160 || h != 80 {
		t.Fatalf("size = %dx%d, want 160x80", w, h)
	}
}

func TestStoredCoverKeepsInkChannels(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(cmykJPEG)
	if err != nil {
		t.Fatal(err)
	}
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	icc := segment(0xe2, []byte("ICC_PROFILE\x00\x01\x01printer profile"))
	out, err := StoredCoverJPEG(jpegWith(data, icc), 8)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	inks, ok := got.(*image.CMYK)
	if !ok || !bytes.Contains(out, icc) {
		t.Fatal("lost ink channels or profile")
	}
	// Each channel is recompressed at thumbnail quality.
	expected := src.(*image.CMYK)
	for ch := range 4 {
		var diff int
		for y := range 8 {
			for x := range 8 {
				a := int(inks.Pix[y*inks.Stride+4*x+ch])
				b := int(expected.Pix[y*expected.Stride+4*x+ch])
				diff += max(a-b, b-a)
			}
		}
		if diff/64 > 14 {
			t.Errorf("channel %d: mean absolute error %d", ch, diff/64)
		}
	}
}

// A CMYK JPEG whose profile names RGB channels: the profile never applied to
// its ink values, so the stored RGB cover is saved without it.
func TestStoredCMYKCoverDropsAnRGBProfile(t *testing.T) {
	t.Parallel()
	data, err := base64.StdEncoding.DecodeString(cmykJPEG)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 128)
	copy(p[16:], "RGB ")
	icc := segment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), p...))
	out, err := StoredCoverJPEG(jpegWith(data, icc), 8)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, cmyk := got.(*image.CMYK); cmyk || bytes.Contains(out, []byte("ICC_PROFILE\x00")) {
		t.Fatalf("stored as %T, profile kept %v; want RGB without the profile", got, bytes.Contains(out, []byte("ICC_PROFILE\x00")))
	}
}

func TestCoverOrientation(t *testing.T) {
	// Row order distinguishes a clockwise turn from a counter-clockwise one
	// and checks both mirror axes. Gray is also used for CMYK's ink planes.
	img := image.NewGray(image.Rect(0, 0, 3, 2))
	copy(img.Pix, "abcdef")
	for _, tc := range []struct {
		orientation int
		width       int
		pixels      string
	}{
		{1, 3, "abcdef"}, {2, 3, "cbafed"}, {3, 3, "fedcba"}, {4, 3, "defabc"},
		{5, 2, "adbecf"}, {6, 2, "daebfc"}, {7, 2, "fcebda"}, {8, 2, "cfbead"},
	} {
		got := orientCover(img, tc.orientation).(*image.Gray)
		if got.Rect.Dx() != tc.width || string(got.Pix) != tc.pixels {
			t.Errorf("orientation %d: %v %q, want width %d and %q", tc.orientation, got.Rect, got.Pix, tc.width, tc.pixels)
		}
		// RGBA pixels move the same way, all four bytes together.
		rgba := image.NewRGBA(img.Rect)
		for i, c := range img.Pix {
			copy(rgba.Pix[4*i:], []byte{c, c + 1, c + 2, 255})
		}
		turned := orientCover(rgba, tc.orientation).(*image.RGBA)
		for i, c := range []byte(tc.pixels) {
			if !bytes.Equal(turned.Pix[4*i:4*i+4], []byte{c, c + 1, c + 2, 255}) || turned.Rect.Dx() != tc.width {
				t.Errorf("orientation %d: RGBA pixel %d = %v, want %q", tc.orientation, i, turned.Pix[4*i:4*i+4], c)
			}
		}
	}
}

func TestStoredPNGCoverKeepsOrientationAndProfile(t *testing.T) {
	img := encodePNG(t, 50, 80, color.White)
	var chunks bytes.Buffer
	add := func(tag string, data []byte) {
		chunks.Write(binary.BigEndian.AppendUint32(nil, uint32(len(data))))
		payload := append([]byte(tag), data...)
		chunks.Write(payload)
		chunks.Write(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(payload)))
	}
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	profile := bytes.Repeat([]byte("profile data"), 6000)
	if _, err := z.Write(profile); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	add("eXIf", exifSegment("II", 8)[10:])
	add("iCCP", append([]byte("profile\x00\x00"), compressed.Bytes()...))
	// After the signature and IHDR chunk.
	src := bytes.Join([][]byte{img[:33], chunks.Bytes(), img[33:]}, nil)
	out, err := StoredCoverJPEG(src, 40)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 40 || h != 25 || !bytes.Contains(out, jpegProfile(profile)) {
		t.Fatalf("stored PNG: %dx%d or missing split profile", w, h)
	}
}

func TestStoredWebPCoverKeepsOrientationAndProfile(t *testing.T) {
	// A 4 x 2 lossless WebP. The extended container adds an ICC and EXIF chunk.
	plain, err := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvA0AAAAcQ0f/+ByKi/wEA")
	if err != nil {
		t.Fatal(err)
	}
	chunk := func(tag string, data []byte) []byte {
		buf := binary.LittleEndian.AppendUint32([]byte(tag), uint32(len(data)))
		buf = append(buf, data...)
		if len(data)%2 != 0 {
			buf = append(buf, 0)
		}
		return buf
	}
	profile := []byte("RGB profile")
	extended := chunk("VP8X", []byte{0x28, 0, 0, 0, 3, 0, 0, 1, 0, 0})
	src := bytes.Join([][]byte{plain[:12], extended, chunk("ICCP", profile), plain[12:], chunk("EXIF", exifSegment("II", 6)[10:])}, nil)
	binary.LittleEndian.PutUint32(src[4:], uint32(len(src)-8))
	out, err := StoredCoverJPEG(src, 4)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, out); w != 2 || h != 4 || !bytes.Contains(out, jpegProfile(profile)) {
		t.Fatalf("stored WebP: %dx%d or missing profile", w, h)
	}
}

// A stored cover keeps a profile only for the channels its JPEG holds, and an
// EXIF block it can't read does not lose the profile after it.
func TestStoredCoverProfileMatchesItsChannels(t *testing.T) {
	t.Parallel()
	profileOf := func(space string) []byte {
		p := make([]byte, 128)
		copy(p[16:], space)
		return segment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), p...))
	}
	badExif := segment(0xE1, []byte("Exif\x00\x00XX\x00\x2a\x00\x00\x00\x08"))
	for _, tc := range []struct {
		name string
		segs [][]byte
		kept bool
	}{
		{"rgb", [][]byte{profileOf("RGB ")}, true},
		{"gray on colour", [][]byte{profileOf("GRAY")}, false},
		{"after a bad exif", [][]byte{badExif, profileOf("RGB ")}, true},
	} {
		icc := tc.segs[len(tc.segs)-1]
		out, err := StoredCoverJPEG(jpegWith(encodeJPEG(t, 16, 8), tc.segs...), 8)
		if err != nil {
			t.Fatal(err)
		}
		if w, h := decodedSize(t, out); w != 8 || h != 4 || bytes.Contains(out, icc) != tc.kept {
			t.Errorf("%s: %dx%d, profile kept %v, want %v", tc.name, w, h, bytes.Contains(out, icc), tc.kept)
		}
	}
}
