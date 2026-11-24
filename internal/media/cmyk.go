package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"

	"golang.org/x/image/draw"
)

// encodeCMYKCover keeps the four ink channels that a printer profile describes.
// Go's JPEG encoder accepts grayscale planes. Each encoded plane supplies one
// sequential scan of a four-component JPEG, using the same quantization tables.
func encodeCMYKCover(src *image.CMYK, w, h, orientation int, icc []byte) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("\xff\xd8")
	out.Write(icc)
	// Adobe APP14, transform 0: inverted CMYK samples.
	out.WriteString("\xff\xee\x00\x0eAdobe\x00\x64\x00\x00\x00\x00\x00")
	plane := image.NewGray(src.Bounds())
	scaled := image.NewGray(image.Rect(0, 0, w, h))
	for ch := range 4 {
		for y := range src.Rect.Dy() {
			row := src.Pix[y*src.Stride:]
			for x := range src.Rect.Dx() {
				plane.Pix[y*plane.Stride+x] = 255 - row[4*x+ch]
			}
		}
		draw.BiLinear.Scale(scaled, scaled.Bounds(), plane, plane.Bounds(), draw.Src, nil)
		data, err := encodeQuality(orientCover(scaled, orientation))
		if err != nil {
			return nil, err
		}
		if err := writeInkScan(&out, data, byte(ch+1)); err != nil {
			return nil, err
		}
	}
	out.WriteString("\xff\xd9")
	return out.Bytes(), nil
}

// writeInkScan reads the headers of a grayscale JPEG made by encodeQuality.
// The first plane supplies the shared tables and frame dimensions. Only the
// component id changes between the four full-resolution scans.
func writeInkScan(out *bytes.Buffer, plane []byte, id byte) error {
	for pos := 2; pos+4 <= len(plane); {
		n := int(binary.BigEndian.Uint16(plane[pos+2:]))
		end := pos + 2 + n
		if n < 2 || end > len(plane) {
			break
		}
		switch plane[pos+1] {
		case 0xdb, 0xc4: // quantization and Huffman tables
			if id == 1 {
				out.Write(plane[pos:end])
			}
		case 0xc0: // baseline frame
			if n != 11 {
				return errors.New("unexpected grayscale JPEG frame")
			}
			if id == 1 {
				out.WriteString("\xff\xc0\x00\x14")
				out.Write(plane[pos+4 : pos+9]) // precision, height, width
				out.WriteByte(4)
				for c := byte(1); c <= 4; c++ {
					out.Write([]byte{c, 0x11, 0})
				}
			}
		case 0xda: // start of scan, followed by entropy-coded data and EOI
			if n != 8 || !bytes.HasSuffix(plane, []byte{0xff, 0xd9}) {
				return errors.New("unexpected grayscale JPEG scan")
			}
			plane[pos+5] = id
			out.Write(plane[pos : len(plane)-2])
			return nil
		}
		pos = end
	}
	return errors.New("missing grayscale JPEG scan")
}
