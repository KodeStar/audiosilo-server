package library

import (
	"cmp"
	"context"
	"iter"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Joined books. A book ripped from CDs often sits in disc folders
// ("Book/CD1", "Book/CD2", the tracks in each, none in "Book"), which the
// folder-per-book model reads as one book per disc. Auto-detection never joins
// them on its own: a book's path is its identity, so silently re-shaping existing
// books would orphan their listeners' progress. A `book` override on such a
// folder is the admin's say-so: the folder becomes ONE book of the audio in its
// disc folders, they make no books of their own, and the scan that applies it
// carries the disc books' state onto the joined book (carryJoinedState).
//
// Only a disc set joins (discSets): a folder with no audio of its own whose every
// folder holding audio beneath it is a disc folder (isDiscFolder: "CD1", "Disc 2")
// directly in it, at least two. Any other folder keeps what `book` meant before
// joining existed (its own files are the book; without any, it reads as nothing),
// so an override set back then, when the detection dialog offered `book` on any
// folder (an author's or a series' among them), still reads the same: a join never
// merges a series or an author's books, nor their listeners' progress. The root
// never joins (joining it would make the whole library one book).

// minDiscs is how many disc folders make a disc set. A single disc folder already
// reads as one book (under its disc name), so it is left alone: the join and the
// Health page's split_discs issue (markSplitDiscs) keep one rule.
const minDiscs = 2

// discSets returns the disc sets among the folders above folders, every
// library-relative folder holding audio of its own that a walk found (the root
// among them or not): each folder below the root with no audio of its own whose
// every folder holding audio beneath it is a disc folder directly in it, at least
// minDiscs. A walk of only part of the library answers for the folders it walked
// from and below. One rule for the join (a `book` override joins a disc set and
// nothing else: joinRoot) and the Health page (the discs of a disc set not yet
// joined: markSplitDiscs, splitParentOf, the console's split_discs).
func discSets(folders iter.Seq[string]) map[string]bool {
	discs := map[string]int{}
	other := map[string]bool{} // folders holding audio, or audio deeper down, that isn't a disc of theirs
	for f := range folders {
		parent := dirOf(f)
		disc := isDiscFolder(path.Base(f))
		for a := f; a != ""; a = dirOf(a) {
			if a == parent && disc {
				discs[a]++
			} else {
				other[a] = true
			}
		}
	}
	sets := map[string]bool{}
	for p, n := range discs {
		if n >= minDiscs && !other[p] {
			sets[p] = true
		}
	}
	return sets
}

// joinRoot returns the joined folder whose book the folder at rel is part of: rel
// itself or the folder holding it (a disc set's audio is only ever in its disc
// folders), when that folder has a `book` override and is a disc set (isDiscSet,
// asked only then). Discovery and IndexPath both decide a join here.
func joinRoot(overrides map[string]string, rel string, isDiscSet func(string) bool) (string, bool) {
	for _, p := range [...]string{dirOf(rel), rel} {
		if p != "" && overrides[p] == catalog.OverrideBook && isDiscSet(p) {
			return p, true
		}
	}
	return "", false
}

// joinedBook builds the book of the joined folder at rootRel from discs, its disc
// folders (library-relative) with their audio files: the folders in natural order
// (discOrder: CD2 before CD10), each folder's files in the order a book of that
// folder alone has them (audioEntries' name order, as the discovery walk reads
// them), so a position on a disc book lands on the same file once offset onto the
// joined book (carryJoinedState). nil when there is no audio.
func joinedBook(lib catalog.Library, rootRel string, discs map[string][]audioFile) *catalog.Book {
	var audio []audioFile
	for _, d := range slices.SortedFunc(maps.Keys(discs), discOrder) {
		audio = append(audio, discs[d]...)
	}
	return folderBook(lib, absOf(lib, rootRel), audio)
}

// markSplitDiscs marks each disc of a disc set not joined with the folder holding
// it (Book.SplitParent, books.split_parent; the Health page's split_discs): a
// folder book whose folder is one of sets (discSets). books are every book a
// folder's subtree makes, as discovery made them; a joined book is no disc (its
// folder holds no audio, so the folder above it is no disc set).
func markSplitDiscs(books []*catalog.Book, sets map[string]bool) {
	for _, b := range books {
		if parent := dirOf(b.RelPath); b.IsFolder && sets[parent] {
			b.SplitParent = parent
		}
	}
}

// dirOf is the library-relative folder holding rel ("" at the root).
func dirOf(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

// isJoined reports whether a folder book reads files from its subfolders.
func isJoined(b *catalog.Book) bool {
	if !b.IsFolder {
		return false
	}
	for _, f := range b.Files {
		if dirOf(f.RelPath) != b.RelPath {
			return true
		}
	}
	return false
}

// joinsIndexed reports whether b, a book new to the index, is a folder joined from
// books already indexed (sigs): its disc folders' books, or a file of one when a
// disc was read as a collection. That is a reshape of books the library had, not
// new content.
func joinsIndexed(b *catalog.Book, sigs map[string]catalog.Signature) bool {
	if !isJoined(b) {
		return false
	}
	for _, f := range b.Files {
		_, disc := sigs[dirOf(f.RelPath)]
		_, file := sigs[f.RelPath]
		if disc || file {
			return true
		}
	}
	return false
}

// splitFrom maps each of books (as discovery found them; found is their paths)
// that came out of a joined book to that book's folder: a book in a disc folder
// (the disc's own book, or a file of it when the disc reads as a collection) of a
// folder whose indexed book (sigs) read its files from its subfolders (isJoined),
// which discovery no longer finds as a book and which didn't move (moves). That is the un-join, when the
// folder's `book` override is removed: the discs are a reshape of a book the
// library had, not new content, and the joined book is split, not removed. Its
// state stays on its path (a disc's own position can't be told from the joined
// book's), for when the folder is joined again. The joinsIndexed counterpart; the
// stored book is read only for a folder that went with books left in its discs.
func (s *Scanner) splitFrom(ctx context.Context, lib catalog.Library, books []*catalog.Book, found map[string]bool,
	sigs map[string]catalog.Signature, moves map[string]string) map[string]string {
	out := map[string]string{}
	joined := map[string]bool{} // a vanished folder book -> whether it was joined
	for _, b := range books {
		disc := b.RelPath
		if !b.IsFolder {
			disc = dirOf(b.RelPath)
		}
		root := dirOf(disc)
		if root == "" || !isDiscFolder(path.Base(disc)) || found[root] || !sigs[root].IsFolder {
			continue
		}
		if _, moved := moves[root]; moved {
			continue
		}
		was, seen := joined[root]
		if !seen {
			old, err := s.cat.GetBookByPath(ctx, lib.ID, root)
			if err != nil {
				s.log.Warn("read split book failed", "library", lib.Name, "path", root, "err", err)
			}
			was = err == nil && isJoined(old)
			joined[root] = was
		}
		if was {
			out[b.RelPath] = root
		}
	}
	return out
}

// joinedPartTitle is the chapter title of a part with no chapters of its own: its
// file name's (partTitle), led by its disc folder's name when the book joins disc
// folders ("CD2 - Track 03"), since disc rips number their tracks from 1 again on
// every disc.
func joinedPartTitle(bookPath, fileRel string) string {
	title := partTitle(fileRel)
	if disc := dirOf(fileRel); disc != bookPath {
		return path.Base(disc) + " - " + title
	}
	return title
}

// discOrder orders the disc folders of a joined book (library-relative, all in
// one folder): by name as naturalCompare reads it ("CD2" before "CD10", "CD 1" with
// "CD1"), then by path, so two folders whose names read alike ("CD01" and "CD1")
// keep a stable order and never interleave their files.
func discOrder(a, b string) int {
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
		if isASCIIDigit(ra[i]) && isASCIIDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && isASCIIDigit(ra[i]) {
				i++
			}
			for j < len(rb) && isASCIIDigit(rb[j]) {
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

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// carryJoinedState hands the durable state of the books a folder now joins (its
// disc books, indexed before the folder's `book` override took effect) to the
// joined book, just before the prune drops them: see catalog.JoinDurableState for
// what moves and what is copied. Only a folder with a `book` override can join, so
// only those are looked at. A book detectMoves already moved has nothing left to
// carry. It returns the paths
// carried from, each logged as joined. When carrying fails (the joined book didn't
// load, or the transaction failed), the books it would have carried from go into
// keep, so the prune leaves them and the next scan tries again; their state stays
// where it was (path-keyed, so nothing is lost) and the failure is logged as an
// error.
func (s *Scanner) carryJoinedState(ctx context.Context, lib catalog.Library, sigs map[string]catalog.Signature,
	overrides map[string]string, keep map[string]bool, moves map[string]string, rl *runLog) map[string]bool {
	carried := map[string]bool{}
	// The books this scan didn't find (and detectMoves didn't move): usually none,
	// so the overrides aren't looked at.
	var gone []string
	for p := range sigs {
		if _, moved := moves[p]; !moved && !keep[p] {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		return carried
	}
	for _, root := range slices.Sorted(maps.Keys(overrides)) {
		if root == "" || overrides[root] != catalog.OverrideBook || !keep[root] {
			continue
		}
		var from []string
		for _, p := range gone {
			if strings.HasPrefix(p, root+"/") {
				from = append(from, p)
			}
		}
		if len(from) == 0 {
			continue
		}
		failed := func(err error) {
			s.log.Warn("join state failed", "library", lib.Name, "path", root, "err", err)
			rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.Detail = root, err.Error() })
			for _, p := range from {
				keep[p] = true
			}
		}
		// The joined book as just indexed: its files carry their lengths. A folder
		// with audio of its own isn't joined; the books under it that went are removed.
		joined, err := s.cat.GetBookByPath(ctx, lib.ID, root)
		if err != nil {
			failed(err)
			continue
		}
		if !isJoined(joined) {
			continue
		}
		parts, total := joinParts(joined, from, sigs)
		if len(parts) == 0 {
			continue
		}
		if err := s.cat.JoinDurableState(ctx, lib.ID, root, parts, total); err != nil {
			failed(err)
			continue
		}
		for _, p := range parts {
			carried[p.Path] = true
			if p.Unplaced {
				s.log.Info("joined into one book; progress stayed with the disc: length unknown",
					"library", lib.Name, "from", p.Path, "to", root)
			} else {
				s.log.Info("joined into one book", "library", lib.Name, "from", p.Path, "to", root)
			}
			rl.add("info", "joined", func(e *catalog.RunEvent) {
				e.Path, e.To = p.Path, root
				if p.Unplaced {
					e.Code = joinLengthUnknown
				}
			})
		}
	}
	return carried
}

// joinLengthUnknown is the code of a `joined` run event for a disc whose state
// stayed on its own path because its offset on the joined book is unknown
// (JoinPart.Unplaced).
const joinLengthUnknown = "length_unknown"

// joinParts places each book being joined (from) on the joined book's timeline:
// its offset is the length of the joined book's files before its first one, its
// length that of its own files (or, when any of its files' lengths is unknown, the
// length the book was indexed with). An offset is known only when every length
// before it is (the first book's, 0, always is); a book whose offset isn't is
// Unplaced (ffprobe off or failing): its state stays where it is rather than land
// in an earlier disc. Last marks the book that ends the joined one. In timeline
// order; a book none of whose files the joined book holds is left out. total is
// the joined book's length (0 when unknown).
func joinParts(joined *catalog.Book, from []string, sigs map[string]catalog.Signature) (parts []catalog.JoinPart, total float64) {
	// A file's book is the innermost holding it ("CD10/Bonus" was a book of its own
	// beside "CD10").
	owner := func(file string) string {
		best := ""
		for _, p := range from {
			if (file == p || strings.HasPrefix(file, p+"/")) && len(p) > len(best) {
				best = p
			}
		}
		return best
	}
	files := joined.Files
	owners := make([]string, len(files)) // each file's book, worked out once
	for i, f := range files {
		owners[i] = owner(f.RelPath)
	}
	at := map[string]int{} // path -> index in parts
	last := ""
	known := true // every length so far is known, so the next offset is
	for i := 0; i < len(files); {
		o := owners[i]
		j, d, complete := i, 0.0, true
		for ; j < len(files) && owners[j] == o; j++ {
			d += files[j].Duration
			complete = complete && files[j].Duration > 0
		}
		// A run with any file of unknown length has no known length of its own (the
		// sum of the rest is short of it, and would place every later disc too early).
		if !complete {
			d = 0
			if o != "" {
				d = sigs[o].Duration
			}
		}
		if o != "" {
			if k, seen := at[o]; seen {
				parts[k].Duration += d
			} else {
				at[o] = len(parts)
				parts = append(parts, catalog.JoinPart{Path: o, Offset: total, Duration: d, Unplaced: !known})
			}
		}
		known = known && d > 0
		last = o
		total += d
		i = j
	}
	if k, ok := at[last]; ok {
		parts[k].Last = true
	}
	if !known {
		total = 0 // the sum of the lengths known is short of the book's
	}
	// Not joined.Duration: that sums the files' lengths only, so it is short
	// whenever a disc's length came from its indexed one.
	return parts, total
}
