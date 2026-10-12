package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The connect and setup pages have no build step, so they carry copies of the
// admin console's theme script and Shelf tokens. These tests fail when the
// console's originals change without the copies.

// readFile returns the file at path (relative to this package) as a string.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestThemeInitMatchesConsole: assets/theme-init.js is admin-ui's, comments aside.
func TestThemeInitMatchesConsole(t *testing.T) {
	t.Parallel()
	code := func(path string) string {
		var lines []string
		for _, l := range strings.Split(readFile(t, path), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "//") {
				lines = append(lines, l)
			}
		}
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
	if ours, console := code("assets/theme-init.js"), code("../../admin-ui/public/theme-init.js"); ours != console {
		t.Errorf("assets/theme-init.js has drifted from admin-ui/public/theme-init.js:\n%s\n---\n%s", ours, console)
	}
}

var tokenRE = regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+):\s*([^;]+);`)

// tokens reads the custom properties of the first block after header in css.
func tokens(t *testing.T, css, header string) map[string]string {
	t.Helper()
	i := strings.Index(css, header)
	if i < 0 {
		t.Fatalf("no %q block", header)
	}
	block := css[i:]
	end := strings.Index(block, "}")
	if end < 0 {
		t.Fatalf("the %q block never closes", header)
	}
	block = block[:end]
	out := map[string]string{}
	for _, m := range tokenRE.FindAllStringSubmatch(block, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// TestStyleTokensMatchConsole: every colour token style.css copies has the value
// the console's globals.css gives it, in light and dark. Font stacks and radii are
// named differently on purpose (no fontsource "Variable" families, fixed px).
func TestStyleTokensMatchConsole(t *testing.T) {
	t.Parallel()
	ours, console := readFile(t, "assets/style.css"), readFile(t, "../../admin-ui/src/styles/globals.css")
	for _, block := range []struct{ ours, console string }{
		{":root {", ":root {"},
		{`[data-theme="dark"] {`, "[data-theme='dark'] {"},
	} {
		want := tokens(t, console, block.console)
		checked := 0
		for name, v := range tokens(t, ours, block.ours) {
			if strings.HasPrefix(name, "--font-") || strings.HasPrefix(name, "--radius") {
				continue
			}
			checked++
			if want[name] != v {
				t.Errorf("%s %s = %q, the console has %q", block.ours, name, v, want[name])
			}
		}
		if checked < 10 {
			t.Errorf("%s: only %d tokens compared; did the block move?", block.ours, checked)
		}
	}
}
