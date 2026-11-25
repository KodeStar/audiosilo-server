//go:build !unix

package fsaccess

import "os"

// owner is unknown where files have no uid and gid (Windows).
func owner(os.FileInfo) (uid, gid int, known bool) { return 0, 0, false }
