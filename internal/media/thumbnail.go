package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"

	// Decoders for the cover formats image.Decode recognizes: every type coverMIME
	// accepts as art. GIF and BMP covers turn up in old ID3 tags; WebP is a
	// custom-cover format.
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxThumbnailSourcePixels bounds the image a thumbnail is decoded from. A
// decoded image costs 3-4 bytes per pixel whatever its file size, so a small file
// claiming huge dimensions (a decompression bomb) is refused from its header,
// before anything is allocated. 40 MP (about 6300 x 6300) is far beyond any real
// cover.
const MaxThumbnailSourcePixels = 40_000_000

// ErrImageTooLarge marks a source image whose dimensions exceed
// MaxThumbnailSourcePixels.
var ErrImageTooLarge = errors.New("image dimensions too large")

// thumbnailQuality is the JPEG quality of a thumbnail: covers are photographs or
// flat art shown small, where 80 is indistinguishable from the source.
const thumbnailQuality = 80

// ThumbnailRev names what Thumbnail makes of the same art, for a thumbnail's
// ETag: change it whenever Thumbnail would return other bytes for the same
// source (its quality, its scaler, which JPEGs it re-encodes), so that a client
// holding the old bytes fetches the new rather than being told its copy is
// current, or, resuming a copy cut short, being sent the rest of other bytes.
const ThumbnailRev = "t2"

// Thumbnail scales cover art down to fit within size x size pixels and encodes it
// as a JPEG. It never scales up: art already within size keeps its own size. A
// JPEG that small is re-encoded at thumbnailQuality and the smaller of that and
// the source's own bytes returned (cover.jpg files are often saved at a quality
// near 95, twice the bytes for a 180 px tile; a re-encode of art saved at a lower
// quality comes out larger and the source is kept), unless it is drawn from more
// than its pixels (ownRendering), which a re-encode would lose: then its own
// bytes. Transparent areas are flattened onto white, as a cover is shown on a
// card.
func Thumbnail(src []byte, size int) ([]byte, error) { return thumbnail(src, size, false) }

// ThumbnailJPEG is Thumbnail that always re-encodes, a small JPEG too: what it
// returns is only ever this server's JPEG encoding of the decoded pixels, never
// the source's bytes (art fetched from a stranger's host).
func ThumbnailJPEG(src []byte, size int) ([]byte, error) { return thumbnail(src, size, true) }

func thumbnail(src []byte, size int, reencode bool) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxThumbnailSourcePixels {
		return nil, ErrImageTooLarge
	}
	small := !reencode && format == "jpeg" && cfg.Width <= size && cfg.Height <= size
	if small && (cfg.ColorModel == color.CMYKModel || ownRendering(src)) {
		// Go's decoder converts CMYK naively, and its encoder writes no
		// orientation or colour profile: a re-encode would show in other colours
		// or turned. The bytes as they are, which a viewer draws right.
		return src, nil
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if small {
		// Served as it is unless a re-encode is smaller: a JPEG Go can't decode
		// fully (truncated, say) is still the browser's to show, as before.
		if err != nil {
			return src, nil
		}
		out, eerr := encodeQuality(img)
		if eerr != nil || len(out) >= len(src) {
			return src, nil
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	w, h := fitWithin(cfg.Width, cfg.Height, size)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	op := draw.Src
	if o, ok := img.(interface{ Opaque() bool }); !ok || !o.Opaque() {
		// Only art that may be transparent needs the white card under it.
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		op = draw.Over
	}
	// BiLinear (the kernel, not ApproxBiLinear) widens its support with the scale
	// factor, so a large downscale averages its source pixels instead of aliasing.
	draw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), op, nil)
	return encodeQuality(dst)
}

// ownRendering reports whether a JPEG carries what a viewer applies when it draws
// it and Go's encoder doesn't write: an EXIF orientation other than 1, or an ICC
// colour profile. It reads the segments before the image data; one it can't
// read counts as carrying them (the bytes are kept).
func ownRendering(src []byte) bool {
	if len(src) < 2 || src[0] != 0xFF || src[1] != 0xD8 {
		return true
	}
	for i := 2; i+4 <= len(src); {
		if src[i] != 0xFF {
			return true
		}
		marker := src[i+1]
		switch {
		case marker == 0xFF: // fill
			i++
			continue
		case marker == 0xDA || marker == 0xD9: // the image data, or its end
			return false
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // no length
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(src[i+2:]))
		if n < 2 || i+2+n > len(src) {
			return true
		}
		seg := src[i+4 : i+2+n]
		switch {
		case marker == 0xE2 && bytes.HasPrefix(seg, []byte("ICC_PROFILE\x00")):
			return true
		case marker == 0xE1 && len(seg) >= 6 && bytes.HasPrefix(seg, []byte("Exif\x00")):
			if o, ok := exifOrientation(seg[6:]); !ok || o > 1 {
				return true
			}
		}
		i += 2 + n
	}
	return true // no image data found
}

// exifOrientation is the Orientation (tag 0x0112) in an EXIF block's first IFD,
// 0 when it has none; false when the block can't be read.
func exifOrientation(tiff []byte) (int, bool) {
	if len(tiff) < 8 {
		return 0, false
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}
	ifd := int64(order.Uint32(tiff[4:]))
	if ifd < 8 || ifd+2 > int64(len(tiff)) {
		return 0, false
	}
	n := int(order.Uint16(tiff[ifd:]))
	for e := int(ifd) + 2; n > 0; e, n = e+12, n-1 {
		if e+12 > len(tiff) {
			return 0, false
		}
		if order.Uint16(tiff[e:]) == 0x0112 {
			return int(order.Uint16(tiff[e+8:])), true
		}
	}
	return 0, true
}

// encodeQuality is img as a JPEG at thumbnailQuality.
func encodeQuality(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fitWithin returns w x h scaled down (never up) so the longer side is at most
// size, keeping the aspect ratio and at least one pixel per side.
func fitWithin(w, h, size int) (int, int) {
	if w <= size && h <= size {
		return w, h
	}
	if w >= h {
		return size, max(1, h*size/w)
	}
	return max(1, w*size/h), size
}

// ReadLimited reads at most limit bytes from r, failing with ErrImageTooLarge
// when there are more (a sidecar image read whole to thumbnail it).
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrImageTooLarge
	}
	return data, nil
}
