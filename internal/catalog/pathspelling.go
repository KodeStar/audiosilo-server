package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	unorm "golang.org/x/text/unicode/norm"
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

// normSensitive reports whether p has another Unicode spelling: false when it
// is both NFC and NFD, as every ASCII path is, so Spelling needs no lookup.
func normSensitive(p string) bool {
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
	where := `path_key IS NULL AND id > ?`
	if all {
		where = `id > ?`
		resume, err := getServerState[pathKeysResume](ctx, c.db, pathKeysResumeKey)
		if err != nil {
			return 0, err
		}
		if resume.Version == pathKeysVersion {
			after = resume.After
		}
	}
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE `+where, after).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 && !all {
		return 0, nil
	}
	keyed := 0
	for {
		n, last, err := c.keyPathsIn(ctx, where, after, all)
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

// keyPathsIn is one of KeyPaths' transactions: the next keyPathsChunk books
// past id after that where admits, keyed. With all (a re-key of every book) it
// records the last id done in the same transaction. It returns how many books
// it read and the last one's id (0 and after when none were left).
func (c *Catalog) keyPathsIn(ctx context.Context, where string, after int64, all bool) (int, int64, error) {
	n, last := 0, after
	err := c.db.WithTx(ctx, "KeyPaths", func(tx *sql.Tx) error {
		type book struct {
			id     int64
			rel    string
			stored sql.NullString
		}
		books, err := queryRows(ctx, tx, func(r *sql.Rows, b *book) error { return r.Scan(&b.id, &b.rel, &b.stored) },
			`SELECT id, rel_path, path_key FROM books WHERE `+where+` ORDER BY id LIMIT ?`, after, cmp.Or(c.chunkKeyPaths, keyPathsChunk))
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
// From the index alone, never the disk:
//
//  1. a path with no other Unicode spelling (every ASCII path) is p, no lookup;
//  2. a book at p exactly, or a file of or folder inside a book (bookHolding),
//     is p;
//  3. the one book whose path is p in NFC (rel_path or path_key);
//  4. a file of the innermost folder book above p, or a folder inside one (a
//     disc folder of a joined book), whose path is p in NFC: that file's
//     spelling, cut to p's depth for a folder;
//  5. a folder holding books (a navigation folder) whose path is p with each
//     name in its given, NFC or NFD spelling: that spelling.
//
// allow is the caller's scope (nil: every path): a spelling it refuses counts
// as none, so the answer never depends on what lies outside it (a third
// spelling of two side by side reads the same whether the one out of reach is
// there or not). Two indexed paths allow admits that both read as p (an NFC and
// an NFD folder side by side on a disk that tells them apart) leave p as given:
// it never guesses. The result is p or an indexed path allow admits; a caller
// still checks scope and hidden paths on it, and reads the disk only through
// SafeJoin.
func (c *Catalog) Spelling(ctx context.Context, libraryID int64, p string, allow func(string) bool) (string, error) {
	if !normSensitive(p) {
		return p, nil
	}
	if _, err := bookIDByPath(ctx, c.db, libraryID, p); !errors.Is(err, ErrNotFound) {
		return p, err // the book at p, or a failed lookup
	}
	// A track or disc folder spelled as the index has it (every seek of a
	// stream asks): one lookup, not the reads of steps 3 and 4.
	if _, _, err := c.bookHolding(ctx, libraryID, p, false); !errors.Is(err, ErrNotFound) {
		return p, err
	}
	k := unorm.NFC.String(p)
	books, err := c.booksKeyed(ctx, libraryID, []string{k}, false)
	if err != nil {
		return "", err
	}
	m := spellMatch{allow: allow}
	for _, b := range books {
		m.add(b.rel)
	}
	if s, found := m.one(p); found {
		return s, nil
	}
	if s, found, err := c.spellingInBook(ctx, libraryID, p, k, allow); found || err != nil {
		return s, err
	}
	return c.folderSpelling(ctx, libraryID, p, allow)
}

// spellMatch gathers the indexed paths one of Spelling's steps finds reading
// as p: those allow admits (nil: every one), each once.
type spellMatch struct {
	allow func(string) bool
	in    []string
}

func (m *spellMatch) add(s string) {
	if (m.allow == nil || m.allow(s)) && !slices.Contains(m.in, s) {
		m.in = append(m.in, s)
	}
}

// one is the step's answer: the one path it found, or p for two (it never
// guesses); found is false when it found none, so the next step looks.
func (m *spellMatch) one(p string) (string, bool) {
	switch len(m.in) {
	case 0:
		return "", false
	case 1:
		return m.in[0], true
	}
	return p, true
}

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
// that holds one allow admits (two books side by side whose folders read as
// one are looked in both). found is false when no book above p holds such a
// path.
func (c *Catalog) spellingInBook(ctx context.Context, libraryID int64, p, k string, allow func(string) bool) (string, bool, error) {
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
		m := spellMatch{allow: allow}
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

// folderSpelling is Spelling's step 5: p as a folder holding indexed books,
// each of its names in the spelling given, NFC or NFD (one FoldersHoldingBooks
// read), the one allow admits. p when the index holds books below p as given,
// or below none or several of the other spellings it admits.
func (c *Catalog) folderSpelling(ctx context.Context, libraryID int64, p string, allow func(string) bool) (string, error) {
	spellings := folderSpellings(p)
	holding, err := c.FoldersHoldingBooks(ctx, libraryID, spellings)
	if err != nil || holding[p] {
		return p, err
	}
	m := spellMatch{allow: allow}
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
