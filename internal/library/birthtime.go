package library

// minBirth is the earliest birth time addedAt takes as one (1980-01-01 UTC, in
// Unix seconds): a filesystem that keeps none may still answer one, macOS over
// an rclone NFS share answering 0 (the epoch: a book added in 1970), and no
// file a library holds was made before personal computers kept the date.
const minBirth = 315532800

// birthKnown reports whether sec, a birth time a stat answered, is one
// (minBirth): one that isn't is unknown, and the mtime stands in for it, as on
// a filesystem that records none.
func birthKnown(sec int64) bool { return sec >= minBirth }
