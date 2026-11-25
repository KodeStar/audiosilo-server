package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
)

// iccChunk is the profile payload of a full JPEG APP2 segment. A profile
// can have up to 255 numbered segments.
const iccChunk = 65535 - 2 - 14

// containerRendering reads PNG and WebP metadata that Go's pixel decoders
// discard. The PNG profile is compressed separately from its image data.
func containerRendering(src []byte, format string, keepProfile bool) (jpegRendering, error) {
	var r jpegRendering
	var pos int
	switch format {
	case "png":
		pos = 8
	case "webp":
		pos = 12
	default:
		return r, nil
	}
	readProfile := false
	for pos+8 <= len(src) {
		var tag string
		var length uint32
		if format == "png" {
			tag = string(src[pos+4 : pos+8])
			length = binary.BigEndian.Uint32(src[pos:])
		} else {
			tag = string(src[pos : pos+4])
			length = binary.LittleEndian.Uint32(src[pos+4:])
		}
		pos += 8
		if uint64(length) > uint64(len(src)-pos) {
			break
		}
		p := src[pos : pos+int(length)]
		pos += len(p)
		if format == "png" {
			pos += 4
		} else {
			pos += len(p) % 2
		}
		switch tag {
		case "IEND":
			return r, nil
		case "eXIf", "EXIF":
			r.orientation, _ = exifOrientation(bytes.TrimPrefix(p, []byte("Exif\x00\x00")))
		case "ICCP":
			if keepProfile && !readProfile && format == "webp" {
				if len(p) > 255*iccChunk {
					return r, ErrImageTooLarge
				}
				readProfile = true
				r.icc = jpegProfile(p)
			}
		case "iCCP":
			if !keepProfile || readProfile || format != "png" {
				continue
			}
			readProfile = true
			_, compressed, ok := bytes.Cut(p, []byte{0})
			if !ok || len(compressed) < 2 || compressed[0] != 0 {
				continue
			}
			z, err := zlib.NewReader(bytes.NewReader(compressed[1:]))
			if err != nil {
				continue
			}
			profile, err := io.ReadAll(io.LimitReader(z, 255*iccChunk+1))
			_ = z.Close()
			if len(profile) > 255*iccChunk {
				return r, ErrImageTooLarge
			}
			if err == nil {
				r.icc = jpegProfile(profile)
			}
		}
	}
	return r, nil
}

func jpegProfile(profile []byte) []byte {
	var segments []byte
	count := (len(profile) + iccChunk - 1) / iccChunk
	for i := range count {
		n := min(len(profile), iccChunk)
		segments = append(segments, 0xff, 0xe2)
		segments = binary.BigEndian.AppendUint16(segments, uint16(2+14+n))
		segments = append(segments, "ICC_PROFILE\x00"...)
		segments = append(segments, byte(i+1), byte(count))
		segments = append(segments, profile[:n]...)
		profile = profile[n:]
	}
	return segments
}
