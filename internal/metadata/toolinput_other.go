//go:build !unix

package metadata

import "os"

// toolInput is ToolInput by path (heldPathInput): no descriptor is inherited
// here.
func toolInput(f *os.File, root, path string) (string, []*os.File, func(), error) {
	return heldPathInput(f, root, path)
}

// toolInputLooks: heldPathInput looks at the share (path's links, an open).
const toolInputLooks = true

// sweepToolInputs: no folder is made for a tool's input here.
func sweepToolInputs() int { return 0 }
