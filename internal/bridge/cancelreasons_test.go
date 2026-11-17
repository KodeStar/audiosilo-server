package bridge

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The console words a server cancel by its reason and keeps a copy of the
// reasons (admin-ui generate-model.ts SERVER_CANCEL_REASONS, BRIDGE-02): a reason
// added or reworded here without the copy would read as a failure there
// (server#270). The two lists hold the same strings.
func TestCancelReasonsMatchConsole(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../../admin-ui/src/features/bridge/generate-model.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const SERVER_CANCEL_REASONS: readonly string\[\] = \[(.*?)\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("SERVER_CANCEL_REASONS not found in generate-model.ts")
	}
	var console []string
	for _, m := range regexp.MustCompile(`'([^']*)'|"([^"]*)"`).FindAllSubmatch(block[1], -1) {
		console = append(console, string(m[1])+string(m[2]))
	}
	// A sorted copy: cancelReasons' slice is shared with IsCancelReason, which
	// the package's parallel tests read meanwhile.
	server := slices.Sorted(slices.Values(cancelReasons()))
	slices.Sort(console)
	if !slices.Equal(console, server) {
		t.Errorf("SERVER_CANCEL_REASONS has drifted from the server's cancel reasons:\nconsole:\n  %s\nserver:\n  %s",
			strings.Join(console, "\n  "), strings.Join(server, "\n  "))
	}
}
