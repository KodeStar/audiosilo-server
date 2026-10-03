package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"

	// Decoders for the cover formats image.Decode recognizes. GIF covers turn up in
	// old ID3 tags; WebP is a custom-cover format.
	_ "image/gif"
	_ "image/png"

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

// Thumbnail scales cover art down to fit within size x size pixels and encodes it
// as a JPEG. It never scales up: art already within size is re-encoded only when
// it isn't a JPEG (a JPEG that small is returned as is). Transparent areas are
// flattened onto white, as a cover is shown on a card.
func Thumbnail(src []byte, size int) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxThumbnailSourcePixels {
		return nil, ErrImageTooLarge
	}
	if format == "jpeg" && cfg.Width <= size && cfg.Height <= size {
		return src, nil
	}
	img, _, err := image.Decode(bytes.NewReader(src))
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
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
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
