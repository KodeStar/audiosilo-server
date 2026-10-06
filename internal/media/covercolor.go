package media

import (
	"fmt"
	"image"
	"image/color"
	"math"
)

// Palette is the colours a player themes a book's screens with, read from its
// cover: Bg is the dominant colour; Accent a vibrant one, its lightness nudged
// until it reads against Bg (a WCAG contrast of at least MinAccentContrast);
// OnAccent the text colour on Accent (white or black, whichever contrasts more).
// All are lowercase "#rrggbb". Accent and OnAccent are "" when the cover has no
// vibrant colour (near-greyscale art) or none can be made to read against Bg.
type Palette struct {
	Bg, Accent, OnAccent string
}

// MinAccentContrast is the WCAG contrast ratio an accent keeps against the
// background: the AA level for normal text.
const MinAccentContrast = 4.5

const (
	// paletteGrid is the most samples read per side: a cover's colours show at
	// 64 x 64 as well as at full size, at a fraction of the reads.
	paletteGrid = 64
	// bucketBits is the bits per channel a colour is quantized to when counting:
	// 16 levels each, so a shade and its JPEG noise land in one bucket.
	bucketBits = 4
	// A vibrant colour is reasonably saturated and mid-lightness (HSL), and covers
	// at least 1/accentMinShare of the samples (a few stray pixels are noise).
	accentMinSat   = 0.3
	accentMinLight = 0.2
	accentMaxLight = 0.8
	accentMinShare = 200
	// The accent's lightness is nudged within these bounds only: beyond them it
	// would be black or white, not a colour of the cover.
	nudgeMinLight = 0.05
	nudgeMaxLight = 0.95
	nudgeStep     = 0.005
)

// rgb8 is an opaque 8-bit colour.
type rgb8 struct{ r, g, b uint8 }

func (c rgb8) hex() string { return fmt.Sprintf("#%02x%02x%02x", c.r, c.g, c.b) }

// bucket accumulates the real colours that quantize to one bucket, so its colour
// is their average rather than the bucket's corner.
type bucket struct {
	n          int
	sr, sg, sb int
}

func (b *bucket) mean() rgb8 {
	return rgb8{uint8((b.sr + b.n/2) / b.n), uint8((b.sg + b.n/2) / b.n), uint8((b.sb + b.n/2) / b.n)}
}

// CoverPalette reads a cover's Palette from its image, sampling at most
// paletteGrid x paletteGrid pixels. Transparent areas count as white (a cover is
// shown on a card, as Thumbnail flattens it). An empty image has no palette.
func CoverPalette(img image.Image) Palette {
	bounds := img.Bounds()
	if bounds.Empty() {
		return Palette{}
	}
	stepX := max(1, (bounds.Dx()+paletteGrid-1)/paletteGrid)
	stepY := max(1, (bounds.Dy()+paletteGrid-1)/paletteGrid)
	var buckets [1 << (3 * bucketBits)]bucket
	total := 0
	for y := bounds.Min.Y + stepY/2; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X + stepX/2; x < bounds.Max.X; x += stepX {
			c := flatten(img.At(x, y))
			const shift = 8 - bucketBits
			i := int(c.r>>shift)<<(2*bucketBits) | int(c.g>>shift)<<bucketBits | int(c.b>>shift)
			bk := &buckets[i]
			bk.n++
			bk.sr += int(c.r)
			bk.sg += int(c.g)
			bk.sb += int(c.b)
			total++
		}
	}

	dominant := 0
	for i := range buckets {
		if buckets[i].n > buckets[dominant].n {
			dominant = i
		}
	}
	bg := buckets[dominant].mean()
	p := Palette{Bg: bg.hex()}

	vibrant, ok := vibrantColor(buckets[:], total)
	if !ok {
		return p
	}
	accent, ok := readableAccent(vibrant, bg)
	if !ok {
		return p
	}
	p.Accent = accent.hex()
	p.OnAccent = "#ffffff"
	if contrast(accent, rgb8{}) > contrast(accent, rgb8{255, 255, 255}) {
		p.OnAccent = "#000000"
	}
	return p
}

// vibrantColor picks the most vibrant bucket: among the reasonably saturated,
// mid-lightness ones with enough samples, the best by saturation and closeness to
// mid-lightness, weighted by population (its square root, so a large muted area
// doesn't outvote a smaller vivid one). false when there is none (near-greyscale
// art).
func vibrantColor(buckets []bucket, total int) (rgb8, bool) {
	var best rgb8
	bestScore := 0.0
	for i := range buckets {
		bk := &buckets[i]
		if bk.n == 0 || bk.n*accentMinShare < total {
			continue
		}
		c := bk.mean()
		_, s, l := toHSL(c)
		if s < accentMinSat || l < accentMinLight || l > accentMaxLight {
			continue
		}
		score := math.Sqrt(float64(bk.n)) * s * s * (1 - math.Abs(l-0.5))
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best, bestScore > 0
}

// readableAccent returns accent, or the nearest colour of the same hue and
// saturation (by HSL lightness, within the nudge bounds) whose contrast against bg
// is at least MinAccentContrast. The check is on the 8-bit colour that is sent, so
// rounding can't take it below the bar. false when no lightness reaches it.
func readableAccent(accent, bg rgb8) (rgb8, bool) {
	if contrast(accent, bg) >= MinAccentContrast {
		return accent, true
	}
	h, s, l := toHSL(accent)
	// Lightness moves luminance monotonically, so the first step that reaches the
	// bar in a direction is the nearest there.
	var found rgb8
	foundDelta := math.Inf(1)
	for _, dir := range []float64{1, -1} {
		for k := 1; ; k++ {
			delta := float64(k) * nudgeStep
			nl := l + dir*delta
			if nl < nudgeMinLight || nl > nudgeMaxLight || delta >= foundDelta {
				break
			}
			if c := fromHSL(h, s, nl); contrast(c, bg) >= MinAccentContrast {
				found, foundDelta = c, delta
				break
			}
		}
	}
	return found, !math.IsInf(foundDelta, 1)
}

// flatten returns c as an opaque 8-bit colour composited onto white.
func flatten(c color.Color) rgb8 {
	r, g, b, a := c.RGBA() // alpha-premultiplied, 16-bit
	under := 0xffff - a
	return rgb8{uint8((r + under) >> 8), uint8((g + under) >> 8), uint8((b + under) >> 8)}
}

// luminance is a colour's WCAG relative luminance.
func luminance(c rgb8) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// contrast is the WCAG contrast ratio of two colours, from 1 to 21.
func contrast(a, b rgb8) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// toHSL converts c to hue (degrees), saturation and lightness (0..1).
func toHSL(c rgb8) (h, s, l float64) {
	r, g, b := float64(c.r)/255, float64(c.g)/255, float64(c.b)/255
	hi, lo := max(r, g, b), min(r, g, b)
	l = (hi + lo) / 2
	d := hi - lo
	if d == 0 {
		return 0, 0, l
	}
	s = d / (1 - math.Abs(2*l-1))
	switch hi {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, s, l
}

// fromHSL converts hue (degrees), saturation and lightness (0..1) to an 8-bit
// colour.
func fromHSL(h, s, l float64) rgb8 {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g = c, x
	case h < 120:
		r, g = x, c
	case h < 180:
		g, b = c, x
	case h < 240:
		g, b = x, c
	case h < 300:
		r, b = x, c
	default:
		r, b = c, x
	}
	to8 := func(v float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, v+m)) * 255)) }
	return rgb8{to8(r), to8(g), to8(b)}
}
