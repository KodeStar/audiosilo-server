package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"

	// Decoders for the cover formats image.Decode recognizes: every type metadata.CoverMIME
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
const ThumbnailRev = "t3"

// Thumbnail scales cover art down to fit within size x size pixels and encodes it
// as a JPEG. It never scales up: art already within size keeps its own size. A
// JPEG that small is re-encoded at thumbnailQuality and the smaller of that and
// the source's own bytes returned (cover.jpg files are often saved at a quality
// near 95, twice the bytes for a 180 px tile; a re-encode of art saved at a lower
// quality comes out larger and the source is kept), unless it is drawn from more
// than its pixels (an orientation or colour profile), which a re-encode would lose: then its own
// bytes. Transparent areas are flattened onto white, as a cover is shown on a
// card.
func Thumbnail(src []byte, size int) ([]byte, error) { return thumbnail(src, size, localThumb) }

// ThumbnailJPEG is Thumbnail that always re-encodes, a small JPEG too: what it
// returns is only ever this server's JPEG encoding of the decoded pixels, never
// the source's bytes (art fetched from a stranger's host).
func ThumbnailJPEG(src []byte, size int) ([]byte, error) { return thumbnail(src, size, remoteThumb) }

// StoredCoverJPEG fits a community cover within size and keeps its colour
// profile. Only the ICC segments are copied into the new JPEG.
func StoredCoverJPEG(src []byte, size int) ([]byte, error) { return thumbnail(src, size, storedCover) }

type thumbnailMode uint8

const (
	localThumb thumbnailMode = iota
	remoteThumb
	storedCover
)

func thumbnail(src []byte, size int, mode thumbnailMode) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxThumbnailSourcePixels {
		return nil, ErrImageTooLarge
	}
	var render jpegRendering
	if format == "jpeg" {
		render = readJPEGRendering(src)
	} else {
		render, err = containerRendering(src, format, mode == storedCover)
		if err != nil {
			return nil, err
		}
	}
	small := mode == localThumb && format == "jpeg" && cfg.Width <= size && cfg.Height <= size
	if small && (cfg.ColorModel == color.CMYKModel || render.orientation > 1 || len(render.icc) > 0 || render.uncertain) {
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
	keepICC := mode == storedCover && len(render.icc) > 0
	space := iccSpace(render.icc)
	if cmyk, ok := img.(*image.CMYK); ok && keepICC {
		if space == "" || space == "CMYK" {
			return encodeCMYKCover(cmyk, w, h, render.orientation, render.icc)
		}
		// A profile naming other channels never applied to the ink values (a
		// viewer drops it), so it is not attached to their RGB conversion.
		keepICC = false
	}
	gray := cfg.ColorModel == color.GrayModel || cfg.ColorModel == color.Gray16Model
	// A profile for other channels than the JPEG holds (a gray one on a gray
	// PNG with alpha, drawn here as colour) is dropped: a viewer would reject
	// it or draw wrong colours.
	want := "RGB "
	if gray {
		want = "GRAY"
	}
	if keepICC && space != "" && space != want {
		keepICC = false
	}
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
	pixels := orientCover(dst, render.orientation)
	if keepICC && gray {
		gray := image.NewGray(pixels.Bounds())
		draw.Draw(gray, gray.Bounds(), pixels, image.Point{}, draw.Src)
		pixels = gray
	}
	out, err := encodeQuality(pixels)
	if err == nil && keepICC {
		withProfile := append([]byte{0xff, 0xd8}, render.icc...)
		out = append(withProfile, out[2:]...)
	}
	return out, err
}

type jpegRendering struct {
	orientation int
	icc         []byte
	uncertain   bool
}

// iccSpace is the colour space ("RGB ", "GRAY", "CMYK") an ICC profile's
// header names, read from its JPEG APP2 segments (marker, length,
// "ICC_PROFILE\x00", sequence number and count, then the profile); "" when
// the first segment is not the profile's first or names another space.
func iccSpace(segments []byte) string {
	const head = 2 + 2 + 12 + 2
	if len(segments) < head+20 || segments[head-2] != 1 {
		return ""
	}
	switch space := string(segments[head+16 : head+20]); space {
	case "RGB ", "GRAY", "CMYK":
		return space
	}
	return ""
}

// readJPEGRendering reads the display metadata before the image data. An EXIF
// block it can't read leaves it uncertain but still reads on, so an ICC
// profile after it is kept.
func readJPEGRendering(src []byte) jpegRendering {
	r := jpegRendering{uncertain: true}
	badExif := false
	for i := 2; i+4 <= len(src); {
		if src[i] != 0xFF {
			return r
		}
		marker := src[i+1]
		switch {
		case marker == 0xFF: // fill
			i++
			continue
		case marker == 0xDA || marker == 0xD9: // the image data, or its end
			r.uncertain = badExif
			return r
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // no length
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(src[i+2:]))
		if n < 2 || i+2+n > len(src) {
			return r
		}
		seg := src[i+4 : i+2+n]
		switch {
		case marker == 0xE2 && len(seg) >= 14 && bytes.HasPrefix(seg, []byte("ICC_PROFILE\x00")):
			r.icc = append(r.icc, src[i:i+2+n]...)
		case marker == 0xE1 && len(seg) >= 6 && bytes.HasPrefix(seg, []byte("Exif\x00")):
			// The pad byte after "Exif\x00" is 0 by the spec, 0xFF from some writers.
			if o, ok := exifOrientation(seg[6:]); ok {
				r.orientation = o
			} else {
				badExif = true
			}
		}
		i += 2 + n
	}
	return r // no image data found
}

// orientCover applies the eight EXIF orientations after scaling, when the
// pixels already fit within the requested size.
func orientCover(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	bounds := image.Rect(0, 0, w, h)
	if orientation >= 5 {
		bounds = image.Rect(0, 0, h, w)
	}
	// The pixels are moved as bytes (a stored cover is millions of them, and
	// At boxes each one): a gray plane's one byte, else RGBA's four.
	var spix, dpix []byte
	var sstride, dstride, bpp int
	var dst image.Image
	if g, ok := src.(*image.Gray); ok {
		d := image.NewGray(bounds)
		spix, sstride, dpix, dstride, bpp, dst = g.Pix, g.Stride, d.Pix, d.Stride, 1, d
	} else {
		s, ok := src.(*image.RGBA)
		if !ok {
			s = image.NewRGBA(image.Rect(0, 0, w, h))
			draw.Draw(s, s.Bounds(), src, src.Bounds().Min, draw.Src)
		}
		d := image.NewRGBA(bounds)
		spix, sstride, dpix, dstride, bpp, dst = s.Pix, s.Stride, d.Pix, d.Stride, 4, d
	}
	for y := range h {
		for x := range w {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = w - 1 - x
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dy = h - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			at := dy*dstride + dx*bpp
			copy(dpix[at:at+bpp], spix[y*sstride+x*bpp:])
		}
	}
	return dst
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
	if order.Uint16(tiff[2:]) != 42 {
		return 0, false
	}
	if ifd < 8 || ifd+2 > int64(len(tiff)) {
		return 0, false
	}
	n := int(order.Uint16(tiff[ifd:]))
	for e := int(ifd) + 2; n > 0; e, n = e+12, n-1 {
		if e+12 > len(tiff) {
			return 0, false
		}
		if order.Uint16(tiff[e:]) == 0x0112 {
			if order.Uint16(tiff[e+2:]) != 3 || order.Uint32(tiff[e+4:]) != 1 {
				return 0, false
			}
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
