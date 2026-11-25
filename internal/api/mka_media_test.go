package api

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/library"
)

// Attachments come from the file beneath the library root. The declared MIME
// type in the Matroska header cannot turn a non-image into a cover.
func TestMatroskaAttachmentCover(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	var picture bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(0, 0, color.White)
	if err := png.Encode(&picture, img); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		data   []byte
		status int
	}{{"Art", picture.Bytes(), 200}, {"Mislabelled", []byte("plain text attachment"), 404}} {
		book := "Author/" + tc.name
		track := filepath.Join(m.lib.Root, filepath.FromSlash(book), "book.mka")
		if err := os.MkdirAll(filepath.Dir(track), 0755); err != nil {
			t.Fatal(err)
		}
		art := filepath.Join(t.TempDir(), "cover.png")
		if err := os.WriteFile(art, tc.data, 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-i", filepath.Join(m.lib.Root, mkaFile), "-map", "0:a:0", "-c", "copy", "-attach", art, "-metadata:s:t", "mimetype=image/png", track).CombinedOutput(); err != nil {
			t.Fatalf("attach: %v: %s", err, out)
		}
		if _, err := library.NewScanner(m.cat, "ffprobe", m.api.log).Scan(t.Context(), *m.lib); err != nil {
			t.Fatal(err)
		}
		b, err := m.cat.GetBookByPath(t.Context(), m.lib.ID, book)
		if err != nil || b.HasCover == nil || *b.HasCover != (tc.status == 200) {
			t.Fatalf("indexed cover for %s: %+v, %v", book, b, err)
		}
		if tc.status == 200 {
			resp, _ := m.do(t, "GET", m.url("cover", book), "", "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("cover without a credential: %d", resp.StatusCode)
			}
		}
		for _, suffix := range []string{"", "&size=96"} {
			resp, body := m.do(t, "GET", m.url("cover", book)+suffix, m.token, "")
			if resp.StatusCode != tc.status {
				t.Fatalf("cover %s%s: %d", book, suffix, resp.StatusCode)
			}
			if tc.status == 200 && suffix == "" && (resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal([]byte(body), tc.data)) {
				t.Fatal("cover differs from the attached PNG")
			}
		}
	}
}

// The fixture's tone rises by 80 Hz each second, giving decoded samples a
// clock independent of the container timestamps. Noise in two other passages
// makes a byte offset proportional to time a poor seek.
func TestMatroskaVBRRangeSeek(t *testing.T) {
	t.Parallel()
	m := newMKAEnv(t)
	file := filepath.Join(m.lib.Root, mkaFile)
	tone := "aevalsrc=0.5*sin(2*PI*(300*t+40*t*t))+0.4*(random(0)-0.5)*(between(t\\,8\\,15)+between(t\\,21\\,28)):s=22050:d=40"
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", tone, "-c:a", "libmp3lame", "-q:a", "3", file).CombinedOutput(); err != nil {
		t.Fatalf("make VBR audio: %v: %s", err, out)
	}
	m.api.ffmpeg = "ffmpeg"
	stream := m.url("stream", mkaFile)
	resp, body := m.do(t, "GET", stream+"&download=1", m.token, "")
	if resp.StatusCode != 200 || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("remux: %d %s", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}
	raw := []byte(body)
	cached := filepath.Join(t.TempDir(), "cached.mp3")
	if err := os.WriteFile(cached, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", cached).Output()
	if err != nil {
		t.Fatal(err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || math.Abs(duration-40) > 0.2 {
		t.Fatalf("duration %q: %v", out, err)
	}
	// The Xing header is in the first MP3 frame after the optional ID3v2 tag.
	start := 0
	if len(raw) > 10 && string(raw[:3]) == "ID3" {
		for _, n := range raw[6:10] {
			start = start<<7 + int(n&127)
		}
		start += 10
	}
	if len(raw) < start+256 {
		t.Fatal("missing first frame")
	}
	i := bytes.Index(raw[start:start+256], []byte("Xing"))
	if i < 0 {
		t.Fatal("remux lacks a Xing header")
	}
	header := raw[start+i+4:]
	if binary.BigEndian.Uint32(header)&7 != 7 || binary.BigEndian.Uint32(header[4:]) == 0 {
		t.Fatal("Xing lacks a frame count, byte count or TOC")
	}
	size := binary.BigEndian.Uint32(header[8:])
	toc := header[12:112]
	for _, sec := range []int{4, 18, 32} {
		pos := float64(sec) * 100 / duration
		n := int(pos)
		frac := float64(toc[n]) + (pos-float64(n))*(float64(toc[n+1])-float64(toc[n]))
		off := start + int(frac*float64(size)/256)
		resp, part := m.doHeaders(t, "GET", stream, m.token, "", map[string]string{"Range": fmt.Sprintf("bytes=%d-", off)})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("range at %ds: %d", sec, resp.StatusCode)
		}
		resp, live := m.do(t, "GET", stream+"&transcode=1&t="+strconv.Itoa(sec), m.token, "")
		if resp.StatusCode != 200 {
			t.Fatalf("transcode at %ds: %d", sec, resp.StatusCode)
		}
		rangeAt, liveAt := tonePosition(t, part), tonePosition(t, live)
		t.Logf("seek %ds: range %.3fs, transcode %.3fs, difference %+.3fs", sec, rangeAt, liveAt, rangeAt-liveAt)
		if math.Abs(rangeAt-liveAt) > 0.4 || math.Abs(liveAt-float64(sec)) > 0.15 {
			t.Fatalf("seek at %ds: range %.3f, transcode %.3f", sec, rangeAt, liveAt)
		}
	}
}

// tonePosition skips the first second so the decoder has filled its MP3 bit
// reservoir after a byte seek, then estimates the starting
// position from positive crossings over a half second of the fixture's chirp.
func tonePosition(t *testing.T, mp3 string) float64 {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-f", "mp3", "-i", "pipe:0", "-t", "2", "-ar", "22050", "-ac", "1", "-f", "s16le", "pipe:1")
	cmd.Stdin = strings.NewReader(mp3)
	pcm, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = 22050, 33075
	if len(pcm) < 2*end {
		t.Fatal("short decoded audio")
	}
	crossings := 0
	for i := begin; i < end; i++ {
		if int16(binary.LittleEndian.Uint16(pcm[2*i:])) > 0 && int16(binary.LittleEndian.Uint16(pcm[2*(i-1):])) <= 0 {
			crossings++
		}
	}
	return (float64(crossings)*2-300)/80 - float64(begin+end)/(2*22050)
}
