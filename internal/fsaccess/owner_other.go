//go:build !unix

package fsaccess

// owner is unknown where files have no uid and gid (Windows).
func owner(string) (uid, gid int, known bool) { return 0, 0, false }
