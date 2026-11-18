package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	unorm "golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Path spelling (server#210). A name with an accent has two Unicode spellings,
// precomposed (NFC: "É") and decomposed (NFD: "E" + a combining accent). Disks
// keep either (an HFS+ Mac wrote NFD; most tools write NFC), and a link typed by
// hand or shared from another system may carry the other one. The index keeps
// every path as the disk spells it (books.rel_path) and compares spellings by
// their NFC form (books.path_key), so a request in another spelling reaches the
// book while everything stored and answered stays the index's spelling.

// pathKeysKey is the server_state row holding the pathKeysVersion the stored
// books.path_key values were computed with.
const pathKeysKey = "path_keys"

// pathKeysVersion names what a stored key was computed with: the key's rule (the
// leading number) and x/text's Unicode tables, so an upgrade that composes a
// character another way keys every book again.
var pathKeysVersion = "1/" + unorm.Version

// pathKey is books.path_key for a book at rel: "" when rel is already NFC (every
// ASCII path), else its NFC form.
func pathKey(rel string) string {
	if unorm.NFC.IsNormalString(rel) {
		return ""
	}
	return unorm.NFC.String(rel)
}

// HasOtherSpelling reports whether p (a path or one name) has another Unicode
// spelling, NFC or NFD: an accented letter, Hangul, kana with a voicing mark.
// False when it is both NFC and NFD, as every ASCII path, a curly apostrophe
// and a Han name are, so Spelling needs no lookup and no listing could find it
// spelled otherwise (library.BookEbook).
func HasOtherSpelling(p string) bool {
	return !unorm.NFC.IsNormalString(p) || !unorm.NFD.IsNormalString(p)
}

// pathKeysResumeKey is the server_state row of a re-key over every book (a
// pathKeysVersion change) under way: the version it keys for and the last book
// id done, so a start stopped part way on goes on from there.
const pathKeysResumeKey = "path_keys_resume"

// pathKeysResume is pathKeysResumeKey's value.
type pathKeysResume struct {
	Version string `json:"version"`
	After   int64  `json:"after"`
}

// keyPathsChunk is how many books one of KeyPaths' transactions keys: small
// enough that the write-ahead log stays a few MB on a library of any size (each
// commit lets it be checkpointed), large enough that 100,000 books take 50.
const keyPathsChunk = 2000

// KeyPaths writes books.path_key for every book that has none (migration 0083
// left every row NULL; a book an older server inserted after a rollback has
// none either), and for every book whenever pathKeysVersion changes. At the
// start, before the server answers (Spelling reads the keys), in transactions of
// keyPathsChunk books in id order, each committed before the next, so the log
// never holds the whole table and a start stopped part way loses nothing: the
// NULL rows are what is left, and a re-key of every book records the last id it
// did (pathKeysResumeKey). The version is stored once every book is keyed.
// progress, when not nil, is told after each transaction how many books are
// done of how many. It returns how many books it keyed.
func (c *Catalog) KeyPaths(ctx context.Context, progress func(done, total int)) (int, error) {
	v, err := getServerState[string](ctx, c.db, pathKeysKey)
	if err != nil {
		return 0, err
	}
	all := v != pathKeysVersion
	var after int64
	if all {
		resume, err := getServerState[pathKeysResume](ctx, c.db, pathKeysResumeKey)
		if err != nil {
			return 0, err
		}
		if resume.Version == pathKeysVersion {
			after = resume.After
		}
	}
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE `+keyPathsWhere(all), after).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 && !all {
		return 0, nil
	}
	keyed := 0
	for {
		n, last, err := c.keyPathsIn(ctx, after, all)
		if err != nil {
			return keyed, err
		}
		keyed += n
		after = last
		if n == 0 {
			break
		}
		if progress != nil {
			progress(keyed, total)
		}
	}
	return keyed, c.db.WithTx(ctx, "KeyPaths", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM server_state WHERE key = ?`, pathKeysResumeKey); err != nil {
			return err
		}
		return putServerState(ctx, tx, pathKeysKey, pathKeysVersion, c.stamp())
	})
}

// keyPathsWhere is the books KeyPaths keys past an id (?): every one with all,
// else those with no key.
func keyPathsWhere(all bool) string {
	if all {
		return `id > ?`
	}
	return `path_key IS NULL AND id > ?`
}

// keyPathsIn is one of KeyPaths' transactions: the next keyPathsChunk books
// past id after (keyPathsWhere), keyed. With all (a re-key of every book) it
// records the last id done in the same transaction. It returns how many books
// it read and the last one's id (0 and after when none were left).
func (c *Catalog) keyPathsIn(ctx context.Context, after int64, all bool) (int, int64, error) {
	n, last := 0, after
	err := c.db.WithTx(ctx, "KeyPaths", func(tx *sql.Tx) error {
		type book struct {
			id     int64
			rel    string
			stored sql.NullString
		}
		books, err := queryRows(ctx, tx, func(r *sql.Rows, b *book) error { return r.Scan(&b.id, &b.rel, &b.stored) },
			`SELECT id, rel_path, path_key FROM books WHERE `+keyPathsWhere(all)+` ORDER BY id LIMIT ?`, after, cmp.Or(c.chunkKeyPaths, keyPathsChunk))
		if err != nil || len(books) == 0 {
			return err
		}
		set, err := tx.PrepareContext(ctx, `UPDATE books SET path_key = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer set.Close()
		for _, b := range books {
			if k := pathKey(b.rel); !b.stored.Valid || b.stored.String != k {
				if _, err := set.ExecContext(ctx, k, b.id); err != nil {
					return err
				}
			}
		}
		n, last = len(books), books[len(books)-1].id
		if !all {
			return nil
		}
		return putServerState(ctx, tx, pathKeysResumeKey, pathKeysResume{Version: pathKeysVersion, After: last}, c.stamp())
	})
	if err != nil {
		return 0, after, err
	}
	return n, last, nil
}

// Spelling is the index's spelling of p, a cleaned library-relative path a
// client sent: p itself when the index holds it as given or holds nothing it
// could mean, else the one indexed path allow admits whose NFC form is p's.
// From the index (indexSpelling), but for one look at the disk (below):
//
//  1. a path with no other Unicode spelling (every ASCII path) is p, no lookup;
//  2. a book at p exactly, a file of or folder inside a book (bookHolding), or
//     a folder holding books (a navigation folder), is p: what the index has as
//     given is never read as another spelling, whatever else reads as it;
//  3. the one book whose path is p in NFC (rel_path or path_key);
//  4. a file of the innermost folder book above p, or a folder inside one (a
//     disc folder of a joined book), whose path is p in NFC: that file's
//     spelling, cut to p's depth for a folder;
//  5. a folder holding books whose path is p with each name in its given, NFC
//     or NFD spelling: that spelling.
//
// allow is the caller's scope (nil: every path): a spelling it refuses counts
// as none, so the answer never depends on what lies outside it (a third
// spelling of two side by side reads the same whether the one out of reach is
// there or not). Two indexed paths allow admits that both read as p (an NFC and
// an NFD folder side by side on a disk that tells them apart) leave p as given:
// it never guesses. The result is p or an indexed path allow admits; a caller
// still checks scope and hidden paths on it, and reads the disk only through
// SafeJoin.
//
// outside reports that p is as given because the only indexed paths reading
// as it are ones allow refuses: p names, in another spelling, something the
// caller can't reach, which a caller whose scope admits p as given (a rule
// written in that spelling) must answer as missing, never by opening p on a
// disk that ignores the difference (server#210).
//
// The one look at the disk: p is never read as another spelling, nor reported
// outside, when it is an entry of its own there (ownEntry: a disk that keeps
// an NFC and an NFD name apart, with p's not indexed yet), so a folder no scan
// has reached is never taken for the indexed one beside it.
func (c *Catalog) Spelling(ctx context.Context, libraryID int64, p string, allow func(string) bool) (spelled string, outside bool, err error) {
	return c.spelling(ctx, libraryID, p, allow, nil)
}

// spelling is Spelling with its look at the disk taken from looks (nil: no
// bound but the look's own wait): a path asked for once looks are spent gets
// the index's answer, as one whose look fails does.
func (c *Catalog) spelling(ctx context.Context, libraryID int64, p string, allow func(string) bool,
	looks *spellLooks) (spelled string, outside bool, err error) {
	s, refused, err := c.indexSpelling(ctx, libraryID, p, allow)
	own := func() bool {
		lctx, done, ok := looks.take(ctx)
		if !ok {
			return false
		}
		defer done()
		return c.ownEntry(lctx, libraryID, p)
	}
	switch {
	case err != nil:
		return "", false, err
	case s != p:
		if own() {
			return p, false, nil
		}
		return s, false, nil
	case len(refused) > 0:
		return p, !own(), nil
	}
	return p, false, nil
}

// spellLooks bounds Spelling's looks at the disk over one request that spells
// many paths (a list replace names up to a thousand): a count, and a time for
// them all, so a share that answers slowly or not at all costs the request a
// few seconds, never its timeout (each look alone may wait spellLookWait, and
// makes several calls). Once either is spent a path gets the index's answer.
// One request's, never shared between goroutines.
type spellLooks struct {
	left  int       // looks still to make
	until time.Time // and when the last may end
}

// A list replace's looks at the disk (listableRefs): this many, within this
// long in all.
const (
	listSpellLooks = 16
	listSpellWait  = 5 * time.Second
)

// newSpellLooks is a request's allowance of n looks, all within wait from now.
func newSpellLooks(n int, wait time.Duration) *spellLooks {
	return &spellLooks{left: n, until: time.Now().Add(wait)}
}

// take claims one look: the context it runs under (ended when the looks' time
// is, so a look under way then is given up on), its release, and false when
// none is left. A nil l always has one, under ctx.
func (l *spellLooks) take(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	if l == nil {
		return ctx, func() {}, true
	}
	if l.left <= 0 || !time.Now().Before(l.until) {
		return nil, nil, false
	}
	l.left--
	lctx, cancel := context.WithDeadline(ctx, l.until)
	return lctx, cancel, true
}

// indexSpelling is Spelling from the index alone: p's spelling, and when it is
// p because only paths allow refuses read as it, those paths (refused).
func (c *Catalog) indexSpelling(ctx context.Context, libraryID int64, p string, allow func(string) bool) (string, []string, error) {
	if !HasOtherSpelling(p) {
		return p, nil, nil
	}
	if _, err := bookIDByPath(ctx, c.db, libraryID, p); !errors.Is(err, ErrNotFound) {
		return p, nil, err // the book at p, or a failed lookup
	}
	// A track or disc folder spelled as the index has it (every seek of a
	// stream asks): one lookup, not the reads of steps 3 and 4.
	if _, _, err := c.bookHolding(ctx, libraryID, p, false); !errors.Is(err, ErrNotFound) {
		return p, nil, err
	}
	// A navigation folder spelled as the index has it, before any other
	// spelling is looked for: a book of another spelling beside it (a disk
	// that keeps the two apart) must not take its place, and ownEntry's look
	// at the disk (a share not mounted at the start, when the re-key asks)
	// may not be there to keep it.
	if holding, err := c.FoldersHoldingBooks(ctx, libraryID, []string{p}); err != nil || holding[p] {
		return p, nil, err
	}
	k := unorm.NFC.String(p)
	books, err := c.booksKeyed(ctx, libraryID, []string{k}, false)
	if err != nil {
		return "", nil, err
	}
	m := &spellMatch{allow: allow}
	for _, b := range books {
		m.add(b.rel)
	}
	if s, found := m.one(p); found {
		return s, nil, nil
	}
	if s, found, err := c.spellingInBook(ctx, libraryID, p, k, m); found || err != nil {
		return s, nil, err
	}
	s, err := c.folderSpelling(ctx, libraryID, p, m)
	if err != nil || s != p || len(m.in) > 0 {
		return s, nil, err
	}
	return p, m.out, nil
}

// spellLookWait bounds each of ownEntry's looks at the disk: a share that
// stopped answering leaves the index's answer standing.
const spellLookWait = 5 * time.Second

// ownEntry reports whether p is an entry of its own on the library's disk:
// each of its names in its folder's listing as given, byte for byte
// (metadata.NamesListed, through the kept listings, c.listed). A disk that
// ignores the Unicode spelling (macOS, SMB) finds the indexed entry by p but
// lists it only as stored, the other spelling (the indexed one: the scan read
// it from that listing), so p is not; a disk that keeps the spellings apart
// lists p only when a folder or file of that spelling is there beside it,
// whatever its file ids say (a share mounted without the server's inode
// numbers, a FUSE mount). Listings only, no stat of a name of p (the library's
// root alone is resolved, as library.SafeJoinListed's): a variant and a
// missing name read the same listings, up to the first name not listed, and a
// folder once while it is kept, so how long the look takes never tells which.
// A listing that fails or is given up on counts as not (the index's answer
// stands), and so does a root that doesn't resolve.
func (c *Catalog) ownEntry(ctx context.Context, libraryID int64, p string) bool {
	if c.spellLooked != nil {
		c.spellLooked()
	}
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil || lib.Root == "" {
		return false
	}
	// The root as library.SafeJoinListed lists below it (absolute, its links
	// resolved), so a folder both look at is read once while it is kept, under
	// one name, however the library's root is written.
	root, err := filepath.Abs(lib.Root)
	if err == nil {
		root, err = metadata.EvalSymlinksBounded(ctx, root, spellLookWait)
	}
	if err != nil {
		return false
	}
	listed, err := metadata.NamesListed(ctx, root, p, func(ctx context.Context, dir string) ([]string, error) {
		return c.listed.Names(ctx, dir, spellLookWait)
	})
	return listed && err == nil
}

// IndexHolds reports whether the index holds p (a cleaned library-relative
// path) byte for byte: a book's path, a folder holding books, or a file of a
// folder book. The scan spells every one of them as its folder lists it, so
// the disk has p as given, as of the last read: a route that opens p itself
// looks at the listings (metadata.NamesListed) only for a path it doesn't hold.
func (c *Catalog) IndexHolds(ctx context.Context, libraryID int64, p string) (bool, error) {
	holding, err := c.FoldersHoldingBooks(ctx, libraryID, []string{p})
	if err != nil || holding[p] {
		return holding[p], err
	}
	if _, _, err := c.bookHolding(ctx, libraryID, p, true); !errors.Is(err, ErrNotFound) {
		return err == nil, err
	}
	return false, nil
}

// spellMatch gathers the indexed paths one of Spelling's steps finds reading
// as p: those allow admits (nil: every one), each once, and those it refuses
// (out), over every step.
type spellMatch struct {
	allow   func(string) bool
	in, out []string
}

func (m *spellMatch) add(s string) {
	switch {
	case m.allow != nil && !m.allow(s):
		if !slices.Contains(m.out, s) {
			m.out = append(m.out, s)
		}
	case !slices.Contains(m.in, s):
		m.in = append(m.in, s)
	}
}

// one is the step's answer: the one path it found, or p for two (it never
// guesses); found is false when it found none, so the next step looks (afresh:
// in is emptied, out kept).
func (m *spellMatch) one(p string) (string, bool) {
	switch len(m.in) {
	case 0:
		return "", false
	case 1:
		return m.in[0], true
	}
	return p, true
}

// next empties in for the next step's (or folder's) matches.
func (m *spellMatch) next() { m.in = m.in[:0] }

// keyedBook is a book booksKeyed found: its stored path and NFC key.
type keyedBook struct {
	id       int64
	rel, key string
}

// booksKeyed reads the books whose path in NFC is one of keys (each an NFC
// path): those stored in NFC by rel_path, the others by path_key. With folders,
// folder books only.
func (c *Catalog) booksKeyed(ctx context.Context, libraryID int64, keys []string, folders bool) ([]keyedBook, error) {
	args := make([]any, 0, 2*len(keys)+2)
	args = append(args, libraryID)
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, libraryID)
	for _, k := range keys {
		args = append(args, k)
	}
	folder := ``
	if folders {
		folder = ` AND is_folder = 1`
	}
	in := placeholders(len(keys))
	// path_key != '' names the partial index's own condition, so it is used.
	return queryRows(ctx, c.db, func(r *sql.Rows, b *keyedBook) error {
		if err := r.Scan(&b.id, &b.rel); err != nil {
			return err
		}
		b.key = unorm.NFC.String(b.rel)
		return nil
	}, `SELECT id, rel_path FROM books WHERE library_id = ? AND rel_path IN (`+in+`)`+folder+`
	    UNION ALL
	    SELECT id, rel_path FROM books WHERE library_id = ? AND path_key IN (`+in+`) AND path_key != ''`+folder,
		args...)
}

// spellingInBook is Spelling's step 4: p (key k, and held as given by no book:
// step 2) as a file of, or a folder inside, the innermost folder book above it
// that holds one m admits (two books side by side whose folders read as one
// are looked in both). found is false when no book above p holds such a
// path.
func (c *Catalog) spellingInBook(ctx context.Context, libraryID int64, p, k string, m *spellMatch) (string, bool, error) {
	keys, err := c.keyedFolders(ctx, libraryID, k)
	if err != nil || len(keys) == 0 {
		return "", false, err
	}
	books, err := c.booksKeyed(ctx, libraryID, keys, true)
	if err != nil || len(books) == 0 {
		return "", false, err
	}
	byKey := map[string][]keyedBook{}
	for _, b := range books {
		byKey[b.key] = append(byKey[b.key], b)
	}
	depth := strings.Count(p, "/") + 1
	for _, key := range keys { // innermost first
		m.next()
		for _, b := range byKey[key] {
			files, err := queryRows(ctx, c.db, func(r *sql.Rows, f *string) error { return r.Scan(f) },
				`SELECT rel_path FROM book_files WHERE book_id = ?`, b.id)
			if err != nil {
				return "", false, err
			}
			for _, f := range files {
				switch fk := unorm.NFC.String(f); {
				case fk == k:
					m.add(f)
				case strings.HasPrefix(fk, k+"/"):
					m.add(strings.Join(strings.SplitN(f, "/", depth+1)[:depth], "/"))
				}
			}
		}
		if s, found := m.one(p); found {
			return s, true, nil
		}
	}
	return "", false, nil
}

// maxFolderSpellings bounds the spellings folderSpelling tries: a path whose
// names have more combinations tries only p, its NFC and its NFD form.
const maxFolderSpellings = 32

// folderSpelling is Spelling's step 5: p (no folder holding books as given:
// step 2) as a folder holding indexed books, each of its names in the spelling
// given, NFC or NFD (one FoldersHoldingBooks read), the one m admits. p when
// the index holds books below none or several of the other spellings it admits.
func (c *Catalog) folderSpelling(ctx context.Context, libraryID int64, p string, m *spellMatch) (string, error) {
	spellings := folderSpellings(p)[1:] // p itself is first
	holding, err := c.FoldersHoldingBooks(ctx, libraryID, spellings)
	if err != nil {
		return p, err
	}
	m.next()
	for _, s := range spellings {
		if holding[s] {
			m.add(s)
		}
	}
	if s, found := m.one(p); found {
		return s, nil
	}
	return p, nil
}

// folderSpellings is every spelling of p whose names are each as given, NFC or
// NFD (p first), or only p, NFC(p) and NFD(p) past maxFolderSpellings.
func folderSpellings(p string) []string {
	names := strings.Split(p, "/")
	forms := make([][]string, len(names))
	total := 1
	for i, n := range names {
		forms[i] = distinct(n, unorm.NFC.String(n), unorm.NFD.String(n))
		total *= len(forms[i])
		if total > maxFolderSpellings {
			return distinct(p, unorm.NFC.String(p), unorm.NFD.String(p))
		}
	}
	out := []string{""}
	for i, f := range forms {
		next := make([]string, 0, len(out)*len(f))
		for _, prefix := range out {
			for _, n := range f {
				if i > 0 {
					n = prefix + "/" + n
				}
				next = append(next, n)
			}
		}
		out = next
	}
	return out
}

// distinct is ss without repeats, in order.
func distinct(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// ancestors is every folder above the cleaned library-relative path p,
// innermost first ("a/b/c" -> "a/b", "a"): slices of p, so linear in its
// length.
func ancestors(p string) []string {
	var out []string
	for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
		out = append(out, p[:i])
	}
	return out
}
