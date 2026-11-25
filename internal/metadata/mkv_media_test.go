package metadata

import "testing"

func TestCoverMIMESniffsTheBytes(t *testing.T) {
	t.Parallel()
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	for name, tc := range map[string]struct {
		data []byte
		mime string
		ok   bool
	}{
		"jpeg":   {jpeg, "image/jpeg", true},
		"png":    {png, "image/png", true},
		"html":   {[]byte("<!doctype html><script>alert(1)</script>"), "", false},
		"script": {[]byte("alert(document.domain)"), "", false},
		"svg":    {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "", false},
	} {
		mime, ok := CoverMIME(tc.data)
		if ok != tc.ok || (ok && mime != tc.mime) {
			t.Errorf("%s: CoverMIME = %q, %v; want %q, %v", name, mime, ok, tc.mime, tc.ok)
		}
	}
}
