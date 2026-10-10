package relpath

import "testing"

// Security-critical: Within is the segment-boundary rule of share scopes and cast
// tokens.
func TestWithin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		root, p string
		want    bool
	}{
		{"Author/Book", "Author/Book", true},
		{"Author/Book", "Author/Book/01.mp3", true},
		{"Author/Book", "Author/Book/CD1/01.mp3", true},
		{"", "anything/at/all", true}, // the library root holds everything
		{"", "", true},
		{"Author/Book", "Author/Book 2", false}, // a sibling sharing the prefix
		{"Author/Book", "Author/Book 2/01.mp3", false},
		{"Author/Book", "Author/Bookish.mp3", false},
		{"Author/Book", "Author", false}, // the folder above
		{"Author/Book", "", false},
		{"Author/Book", "Other/Author/Book", false},
	} {
		if got := Within(c.root, c.p); got != c.want {
			t.Errorf("Within(%q, %q) = %v, want %v", c.root, c.p, got, c.want)
		}
	}
}
