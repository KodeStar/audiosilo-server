package media

import (
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"testing"
)

// bands returns a w x h image of horizontal bands, each colour taking its share
// of the rows (shares sum to 1).
func bands(w, h int, colors []color.NRGBA, shares []float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	row := 0
	for i, c := range colors {
		end := row + int(math.Round(shares[i]*float64(h)))
		if i == len(colors)-1 {
			end = h
		}
		for y := row; y < end; y++ {
			for x := range w {
				img.SetNRGBA(x, y, c)
			}
		}
		row = end
	}
	return img
}

func parseHex(t *testing.T, s string) rgb8 {
	t.Helper()
	if !regexp.MustCompile(`^#[0-9a-f]{6}$`).MatchString(s) {
		t.Fatalf("%q is not a lowercase #rrggbb colour", s)
	}
	v, _ := strconv.ParseUint(s[1:], 16, 32)
	return rgb8{uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

// checkPalette asserts the palette's invariants: well-formed colours, an accent
// that reads against the background, and the better of white or black on it.
func checkPalette(t *testing.T, p Palette) {
	t.Helper()
	bg := parseHex(t, p.Bg)
	if p.Accent == "" {
		if p.OnAccent != "" {
			t.Fatalf("on_accent %q without an accent", p.OnAccent)
		}
		return
	}
	accent := parseHex(t, p.Accent)
	if c := contrast(accent, bg); c < minAccentContrast {
		t.Fatalf("accent %s on bg %s: contrast %.2f, want >= %.1f", p.Accent, p.Bg, c, minAccentContrast)
	}
	white, black := contrast(accent, rgb8{255, 255, 255}), contrast(accent, rgb8{})
	want := "#ffffff"
	if black > white {
		want = "#000000"
	}
	if p.OnAccent != want {
		t.Fatalf("on_accent = %q for accent %s, want %s", p.OnAccent, p.Accent, want)
	}
}

func TestCoverPaletteSolidColour(t *testing.T) {
	t.Parallel()
	blue := color.NRGBA{30, 90, 200, 255}
	p := coverPalette(bands(300, 300, []color.NRGBA{blue}, []float64{1}))
	if p.Bg != "#1e5ac8" {
		t.Fatalf("bg = %s, want #1e5ac8", p.Bg)
	}
	// The only vibrant colour is the background itself, so the accent is it
	// nudged in lightness until it reads: same hue, not the same colour.
	checkPalette(t, p)
	if p.Accent == "" || p.Accent == p.Bg {
		t.Fatalf("accent = %q, want the blue nudged off the background", p.Accent)
	}
	h, _, _ := toHSL(parseHex(t, p.Accent))
	if wantH, _, _ := toHSL(rgb8{30, 90, 200}); math.Abs(h-wantH) > 3 {
		t.Fatalf("accent hue %.0f, want about %.0f (hue kept)", h, wantH)
	}
}

func TestCoverPaletteTwoColours(t *testing.T) {
	t.Parallel()
	navy, orange := color.NRGBA{20, 30, 80, 255}, color.NRGBA{240, 140, 20, 255}
	p := coverPalette(bands(100, 100, []color.NRGBA{navy, orange}, []float64{0.7, 0.3}))
	// Navy is the most populous; orange is the vibrant one and already reads on it.
	want := Palette{Bg: "#141e50", Accent: "#f08c14", OnAccent: "#000000"}
	if p != want {
		t.Fatalf("palette = %+v, want %+v", p, want)
	}
}

func TestCoverPaletteGreyscaleHasNoAccent(t *testing.T) {
	t.Parallel()
	img := image.NewGray(image.Rect(0, 0, 256, 256))
	for y := range 256 {
		for x := range 256 {
			img.SetGray(x, y, color.Gray{Y: uint8(x)})
		}
	}
	p := coverPalette(img)
	if p.Bg == "" || p.Accent != "" || p.OnAccent != "" {
		t.Fatalf("greyscale palette = %+v, want a bg and no accent", p)
	}
	parseHex(t, p.Bg)
}

// A vibrant colour too close in luminance to the background is nudged lighter or
// darker, keeping its hue and saturation, until it reaches the contrast bar.
func TestCoverPaletteNudgesLowContrastAccent(t *testing.T) {
	t.Parallel()
	slate, red := color.NRGBA{50, 50, 60, 255}, color.NRGBA{200, 60, 60, 255}
	p := coverPalette(bands(100, 100, []color.NRGBA{slate, red}, []float64{0.7, 0.3}))
	if p.Bg != "#32323c" {
		t.Fatalf("bg = %s, want #32323c", p.Bg)
	}
	if c := contrast(rgb8{200, 60, 60}, rgb8{50, 50, 60}); c >= minAccentContrast {
		t.Fatalf("fixture red already has contrast %.2f; the test needs one below the bar", c)
	}
	checkPalette(t, p)
	if p.Accent == "" || p.Accent == "#c83c3c" {
		t.Fatalf("accent = %q, want the red nudged", p.Accent)
	}
	h, s, _ := toHSL(parseHex(t, p.Accent))
	wh, ws, _ := toHSL(rgb8{200, 60, 60})
	if math.Abs(h-wh) > 3 || math.Abs(s-ws) > 0.05 {
		t.Fatalf("accent hsl %.0f/%.2f, want about %.0f/%.2f", h, s, wh, ws)
	}
}

// On a mid-grey background no lightness of a vivid red reaches the bar within the
// nudge bounds: the accent is left out rather than turned black or white.
func TestCoverPaletteOmitsUnreadableAccent(t *testing.T) {
	t.Parallel()
	grey, red := color.NRGBA{117, 117, 117, 255}, color.NRGBA{220, 30, 30, 255}
	p := coverPalette(bands(100, 100, []color.NRGBA{grey, red}, []float64{0.7, 0.3}))
	if p != (Palette{Bg: "#757575"}) {
		t.Fatalf("palette = %+v, want only bg #757575", p)
	}
}

func TestCoverPaletteTransparentIsWhite(t *testing.T) {
	t.Parallel()
	p := coverPalette(image.NewNRGBA(image.Rect(0, 0, 10, 10)))
	if p.Bg != "#ffffff" {
		t.Fatalf("bg = %s, want #ffffff (flattened onto white)", p.Bg)
	}
	if (coverPalette(image.NewNRGBA(image.Rectangle{})) != Palette{}) {
		t.Fatal("an empty image should have no palette")
	}
}

// The contrast guarantee holds for any art: random blocks of random colours.
func TestCoverPaletteContrastProperty(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	accents := 0
	for range 300 {
		n := 1 + rng.IntN(5)
		colors := make([]color.NRGBA, n)
		shares := make([]float64, n)
		for i := range colors {
			colors[i] = color.NRGBA{uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)), 255}
			shares[i] = 1 / float64(n)
		}
		p := coverPalette(bands(80+rng.IntN(400), 80+rng.IntN(400), colors, shares))
		checkPalette(t, p)
		if p.Accent != "" {
			accents++
		}
	}
	if accents < 100 {
		t.Fatalf("only %d of 300 random covers got an accent; the property is barely exercised", accents)
	}
}

// PaletteOf reads the palette from a thumbnail's JPEG, and refuses what is not
// one.
func TestPaletteOf(t *testing.T) {
	t.Parallel()
	thumb, err := Thumbnail(encodePNG(t, 1200, 900, color.NRGBA{200, 30, 90, 255}), 320)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PaletteOf(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if p.Bg != "#c81e5a" {
		t.Fatalf("bg = %s, want #c81e5a", p.Bg)
	}
	checkPalette(t, p)
	if _, err := PaletteOf(encodePNG(t, 10, 10, color.NRGBA{0, 0, 0, 255})); err == nil {
		t.Fatal("PaletteOf read a PNG; want an error")
	}
}
