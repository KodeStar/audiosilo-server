package metadata

import (
	"cmp"
	"path"
	"strings"
)

// discSeparators are dropped from a folder name before IsDiscFolder reads it.
var discSeparators = strings.NewReplacer(" ", "", "-", "", ".", "", "_", "")

// IsDiscFolder reports whether a folder name is a disc/part subfolder like
// "CD1", "CD 2" or "Disc 3": the scan joins such folders into the book above
// them (library: a multi-CD book's cover and files), and an Audible backup's
// destination may not make a new one inside an indexed book.
func IsDiscFolder(name string) bool {
	n := discSeparators.Replace(strings.ToLower(strings.TrimSpace(name)))
	for _, p := range []string{"cd", "disc", "disk"} {
		if !strings.HasPrefix(n, p) {
			continue
		}
		rest := n[len(p):]
		if rest == "" {
			return true
		}
		allDigits := true
		for _, r := range rest {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}

// DiscOrder orders the disc folders of a joined book (library-relative, all in
// one folder): by name as naturalCompare reads it ("CD2" before "CD10", "CD 1" with
// "CD1"), then by path, so two folders whose names read alike ("CD01" and "CD1")
// keep a stable order and never interleave their files. The scanner joins discs
// in this order and Health lists a split book by its first disc in it.
func DiscOrder(a, b string) int {
	if c := naturalCompare(path.Base(a), path.Base(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// naturalCompare compares two names the way people number things: case folded,
// runs of digits by their value ("2" before "10", "01" equal to "1"), and spaces,
// hyphens and underscores left out ("CD 2" sorts with "CD2").
func naturalCompare(a, b string) int {
	ra, rb := naturalRunes(a), naturalRunes(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if isDigit(ra[i]) && isDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return cmp.Compare(len(na), len(nb))
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if ra[i] != rb[j] {
			return cmp.Compare(ra[i], rb[j])
		}
		i++
		j++
	}
	return cmp.Compare(len(ra)-i, len(rb)-j)
}

// naturalRunes is a name as naturalCompare reads it.
func naturalRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		if r == ' ' || r == '-' || r == '_' || r == '\t' {
			continue
		}
		out = append(out, r)
	}
	return out
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
