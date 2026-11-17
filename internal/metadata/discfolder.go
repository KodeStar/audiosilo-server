package metadata

import "strings"

// IsDiscFolder reports whether a folder name is a disc/part subfolder like
// "CD1", "CD 2" or "Disc 3": the scan joins such folders into the book above
// them (library: a multi-CD book's cover and files), and an Audible backup's
// destination may not make a new one inside an indexed book.
func IsDiscFolder(name string) bool {
	n := strings.NewReplacer(" ", "", "-", "", ".", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(name)))
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
