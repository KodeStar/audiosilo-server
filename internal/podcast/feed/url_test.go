package feed

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestURLVectors(t *testing.T) {
	raw, err := os.ReadFile("../testdata/url-vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		v := strings.Split(line, "\t")
		if len(v) != 3 {
			t.Fatal(line)
		}
		norm, err := NormalizeFeedURL(v[0])
		if err != nil || norm != v[1] || FeedURLHash(norm) != v[2] {
			t.Fatalf("%s => %s %v", v[0], norm, err)
		}
	}
	for _, bad := range []string{"ftp://x/y", "/relative", "https:///x", "https://x/" + strings.Repeat("a", 2040), "https://example.com/%xx"} {
		if _, err := NormalizeFeedURL(bad); !errors.Is(err, ErrInvalidFeedURL) {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, tc := range []struct {
		in, norm, redacted string
		private            bool
	}{
		{" https://Example.COM.:443/a%2Fb?x=1&z=2#fragment ", "https://example.com/a%2Fb?x=1&z=2", "https://example.com/...", true},
		{"http://example.com", "http://example.com/", "http://example.com", false},
		{"http://[2001:db8::1]:80/", "http://[2001:db8::1]/", "http://[2001:db8::1]", false},
		{"https://user:pa%40ss@Example.com/feed", "https://user:pa%40ss@example.com/feed", "https://example.com/...", true},
		{"https://a.supercast.tech/feed", "https://a.supercast.tech/feed", "https://a.supercast.tech/...", true},
		{"https://x.substack.com/private/feed", "https://x.substack.com/private/feed", "https://x.substack.com/...", true},
		{"https://example.com/abcdefghijk1234567890ABCDE/rss", "https://example.com/abcdefghijk1234567890ABCDE/rss", "https://example.com/...", true},
		{"https://evilpatreon.com/rss", "https://evilpatreon.com/rss", "https://evilpatreon.com/...", false},
	} {
		norm, err := NormalizeFeedURL(tc.in)
		if err != nil || norm != tc.norm || RedactURL(norm) != tc.redacted || IsPrivateFeedURL(norm) != tc.private {
			t.Errorf("%s => %s %s %t (%v)", tc.in, norm, RedactURL(norm), IsPrivateFeedURL(norm), err)
		}
	}
}
