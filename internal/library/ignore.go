package library

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Ignore rules: per-library patterns naming files and folders the scanner skips
// (sample clips, an "Extras" folder, a podcast feed someone dropped in). They live
// in the database (libraries.ignore_patterns), not in a file in the library, since
// the server never writes to the library folder.
//
// One pattern per line; blank lines and lines starting with # are skipped. A
// pattern without a "/" matches a file or folder name at any depth ("*.sample.mp3",
// "Extras"); a pattern with one is matched against the whole path from the library
// root ("Podcasts/*", a leading "/" is optional). A trailing "/" limits it to
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
	anchored bool   // contains a "/": matched against the whole relative path
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
	for _, l := range lines {
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
	r := ignoreRule{dirOnly: strings.HasSuffix(l, "/")}
	p := strings.Trim(strings.ToLower(l), "/")
	r.anchored = strings.Contains(p, "/")
	r.pattern = p
	return r
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
