package library

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Ignore rules: per-library patterns naming files and folders the scanner skips
// (sample clips, an "Extras" folder, a podcast feed someone dropped in). They live
// in the database (libraries.ignore_patterns), not in a file in the library, since
// the server never writes to the library folder.
//
// One pattern per line; blank lines and lines starting with # are skipped. A
// pattern without a "/" matches a file or folder name at any depth ("*.sample.mp3",
// "Extras"); a pattern with one before its end is matched against the whole path
// from the library root ("Podcasts/*", and "/Extras" for the root's Extras only; a
// leading "/" is optional when there is another). A trailing "/" limits it to
// folders. Matching uses path.Match wildcards (* ? [...]) and ignores case. An
// ignored folder is skipped with everything under it.

const (
	maxIgnorePatterns = 100
	maxIgnorePattern  = 200 // bytes
)

// ErrInvalidIgnore marks an ignore pattern list that can't be used: too many
// patterns, one too long, or a malformed wildcard.
var ErrInvalidIgnore = errors.New("invalid ignore pattern")

type ignoreRule struct {
	pattern  string // lower-cased, without the leading or trailing "/"
	anchored bool   // has a "/" before its end: matched against the whole relative path
	dirOnly  bool
}

// Ignore is a parsed ignore list. The zero value (and nil) ignores nothing.
type Ignore struct{ rules []ignoreRule }

// parseLine reads one stored line: skip for a blank line or a comment, else the
// rule or why it can't be used.
func parseLine(l string) (r ignoreRule, skip bool, err error) {
	if l == "" || strings.HasPrefix(l, "#") {
		return r, true, nil
	}
	if len(l) > maxIgnorePattern {
		return r, false, fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidIgnore, l, maxIgnorePattern)
	}
	r = parseRule(l)
	if r.pattern == "" {
		return r, false, fmt.Errorf("%w: %q matches nothing", ErrInvalidIgnore, l)
	}
	if _, err := path.Match(r.pattern, ""); err != nil {
		return r, false, fmt.Errorf("%w: %q: %v", ErrInvalidIgnore, l, err)
	}
	return r, false, nil
}

// NormalizeIgnore cleans an ignore list for storage: trims each line, drops blank
// lines, and validates every pattern (ErrInvalidIgnore, naming the line). Comments
// are kept.
func NormalizeIgnore(lines []string) ([]string, error) {
	out := []string{}
	n := 0
	// The list is stored one pattern per line, so an entry holding a line break is
	// several patterns, each validated.
	var split []string
	for _, l := range lines {
		split = append(split, strings.FieldsFunc(l, func(r rune) bool { return r == '\n' || r == '\r' })...)
	}
	for _, l := range split {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		_, skip, err := parseLine(l)
		if err != nil {
			return nil, err
		}
		if !skip {
			if n++; n > maxIgnorePatterns {
				return nil, fmt.Errorf("%w: more than %d patterns", ErrInvalidIgnore, maxIgnorePatterns)
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// ParseIgnore builds an Ignore from stored lines. Lines NormalizeIgnore would
// reject are skipped, so a bad row can never stop a scan.
func ParseIgnore(lines []string) *Ignore {
	ig := &Ignore{}
	for _, l := range lines {
		if r, skip, err := parseLine(strings.TrimSpace(l)); !skip && err == nil {
			ig.rules = append(ig.rules, r)
		}
	}
	return ig
}

func parseRule(l string) ignoreRule {
	lower := strings.ToLower(l)
	return ignoreRule{
		pattern: strings.Trim(lower, "/"),
		// Anchored by a "/" anywhere but at the end (which only means "folders"), so a
		// leading one counts: "/Extras" is the root's Extras, not every Extras.
		anchored: strings.Contains(strings.TrimRight(lower, "/"), "/"),
		dirOnly:  strings.HasSuffix(l, "/"),
	}
}

// Empty reports whether the list ignores nothing.
func (ig *Ignore) Empty() bool { return ig == nil || len(ig.rules) == 0 }

// Match reports whether the library-relative path rel (slash-separated) is ignored
// itself, not counting its parent folders (the scanner never descends into an
// ignored folder; Covers checks the parents too).
func (ig *Ignore) Match(rel string, isDir bool) bool {
	if ig.Empty() || rel == "" {
		return false
	}
	lower := strings.ToLower(rel)
	base := path.Base(lower)
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		subject := base
		if r.anchored {
			subject = lower
		}
		if ok, _ := path.Match(r.pattern, subject); ok {
			return true
		}
	}
	return false
}

// ignoresAll reports whether the rules skip every indexed book (sigs, by path):
// then a scan that discovers nothing has found what the rules say, not an
// unmounted share.
func ignoresAll(ig *Ignore, sigs map[string]catalog.Signature) bool {
	if ig.Empty() {
		return false
	}
	for p, sig := range sigs {
		if !ig.Covers(p, sig.IsFolder) {
			return false
		}
	}
	return true
}

// Covers reports whether rel or any folder above it is ignored: what decides
// whether something reached by path (a browse, an on-demand index) is in the
// library at all.
func (ig *Ignore) Covers(rel string, isDir bool) bool {
	if ig.Empty() || rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		last := i == len(parts)-1
		if ig.Match(strings.Join(parts[:i+1], "/"), !last || isDir) {
			return true
		}
	}
	return false
}
