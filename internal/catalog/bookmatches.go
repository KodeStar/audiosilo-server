package catalog

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"unicode"
)

// How a book's community match was made (book_matches.how, migration 0069).
const (
	// MatchHowAuto is a match run's confident item, applied unasked.
	MatchHowAuto = "auto"
	// MatchHowAccepted is a match the admin chose: the match dialog, or a review
	// item included in a run's apply.
	MatchHowAccepted = "accepted"
)

// maxMatchID bounds (in characters) a community work or recording id an anchor
// stores, as the api's validWorkID bounds GET /meta/work's.
const maxMatchID = 200

// MatchAnchor is a book's community match as an edit writes it (BookEdit.Match):
// the community work, the recording (may be "") and how it was made (MatchHowAuto
// or MatchHowAccepted, "" for accepted), with the match run that applied it (0 for
// none). An empty WorkID unlinks the book (BookEdit.Match). Only a community edit
// sets one. OfferedASIN and OfferedISBN are the identifiers the match offered,
// ticked or not ("" for none, or a match recorded before 0070): a refresh by the
// recorded match never fills one a book with neither has when it is the one
// offered (the admin unticked it, or reverted it since; server#330). One the
// book already holds of its own when the match is written (its files', an
// Audiobookshelf file's, an enrichment's or an admin's, anything but a
// community override) is not recorded as offered: nobody chose to leave it
// out, so a refresh fills it once the book loses it (a retag; server#355).
//
// How, RunID, the editor and the time are the match's provenance: who made it
// and when ("matched on"). A run's bookkeeping of a match it read (BookEdit.
// MatchIf naming one: a refresh's heal or new identifier, a repick's ASIN)
// rewrites only the work, recording and offered identifiers and keeps them, so
// a refresh filling an identifier never moves "matched on" and a repick never
// takes the match's run (server#355). A heal to another work id (a merged
// work's) moves the time alone: the upcoming job reads a match's change of
// work by it.
type MatchAnchor struct {
	WorkID      string
	RecordingID string
	How         string
	RunID       int64
	OfferedASIN string
	OfferedISBN string
}

// BookMatch is a book's community match as the console's book page shows it
// (AdminBookDetail.CommunityMatch): the work and recording, how and when it was
// made, and the work's page on the community site (WebURL, set by the api).
type BookMatch struct {
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id,omitempty"`
	How         string `json:"how"`
	MatchedAt   string `json:"matched_at"`
	WebURL      string `json:"web_url,omitempty"`
}

// invisible reports whether r shows nothing: white space, a format character
// (U+200B ZERO WIDTH SPACE, U+FEFF, U+2060 WORD JOINER, ...), a variation
// selector, another default-ignorable code point (U+3164 HANGUL FILLER, U+115F,
// U+1160, U+FFA0) or U+2800 BRAILLE PATTERN BLANK. An id is trimmed of them at
// both ends, so one made only of them is empty, never an id nobody can see.
func invisible(r rune) bool {
	return unicode.IsSpace(r) ||
		unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) ||
		r == '\u2800'
}

// normalizeAnchor validates an edit's anchor (nil = none) and returns it in stored
// form, its ids trimmed of invisible characters at both ends (interior ones are
// kept; checkText refuses control characters). Setting one takes a community
// edit (source, normalized); removing one (an explicitly empty WorkID) any edit.
// A WorkID that is not empty but invisible only is refused, never an Unlink. The
// offered identifiers are stored as the ASIN and ISBN fields store theirs; one
// they would refuse is invalid (field "match"); an Unlink ignores them.
func normalizeAnchor(a *MatchAnchor, source string) (*MatchAnchor, error) {
	if a == nil {
		return nil, nil
	}
	out := *a
	out.WorkID, out.RecordingID = strings.TrimFunc(a.WorkID, invisible), strings.TrimFunc(a.RecordingID, invisible)
	if out.WorkID == "" {
		if a.WorkID != "" {
			return nil, invalid("match", "only an explicitly empty work_id unlinks a book (this one holds only blank or invisible characters)")
		}
		return &MatchAnchor{}, nil
	}
	if source != SourceCommunity {
		return nil, invalid("match", "only a community match records one")
	}
	// Bounded and free of control characters, as every field an edit sets: an id
	// is logged and sent upstream.
	for _, id := range []string{out.WorkID, out.RecordingID} {
		if err := checkText("match", id, maxMatchID); err != nil {
			return nil, err
		}
	}
	for _, o := range []struct {
		field string
		value *string
	}{{FieldASIN, &out.OfferedASIN}, {FieldISBN, &out.OfferedISBN}} {
		v, err := normalizeOverride(o.field, *o.value)
		if oe, ok := errors.AsType[*OverrideError](err); ok {
			return nil, invalid("match", "the offered "+o.field+" "+oe.Reason)
		} else if err != nil {
			return nil, err
		}
		*o.value = v
	}
	switch out.How {
	case "":
		out.How = MatchHowAccepted
	case MatchHowAuto, MatchHowAccepted:
	default:
		return nil, invalid("match", `how must be "auto" or "accepted"`)
	}
	return &out, nil
}

// ErrMatchedSince marks an edit with a MatchIf whose book's recorded match is no
// longer the one it names (matched again by hand, or unlinked, since the caller
// read it), or, for a zero MatchIf, whose book has an ASIN or ISBN now: nothing
// of the edit was written.
var ErrMatchedSince = errors.New("the book's community match changed since it was read")

// writeAnchor stores the book's anchor at ref (a non-empty WorkID; an unlink is
// clearCommunity's) in the edit's transaction. With was (BookEdit.MatchIf), only
// while the recorded match is still was: else it writes nothing and returns
// ErrMatchedSince, so the caller's transaction rolls back. A match it replaces
// with another work sets the book's series due (setMatchedSeriesDue), as an
// unlink does: the job reads a match naming the work the book's identifiers
// resolved to as standing in for nothing, so a re-match to that work would
// leave the series resolved by the old match's until the monthly recheck.
//
// The match is part of the book's community chapter basis (chapterBasisExpr),
// so writing one makes the book's check stale, except a check that already
// came from the recording it names, which is carried over the write
// (checkIsOf). The caller rebuilds the book (refreshEffective), which takes a
// check left stale out of its chapters.
func writeAnchor(ctx context.Context, tx *sql.Tx, ref Ref, a, was *MatchAnchor, editor any, now string) error {
	a, err := withoutOwnOffered(ctx, tx, ref, a)
	if err != nil {
		return err
	}
	if was == nil || was.WorkID != "" { // an insert-only write replaces no match
		if err := setMatchedSeriesDue(ctx, tx, oneBook(ref), a.WorkID); err != nil {
			return err
		}
	}
	holds, err := checkIsOf(ctx, tx, ref, a.WorkID, a.RecordingID)
	if err != nil {
		return err
	}
	if err := storeAnchor(ctx, tx, ref, a, was, editor, now); err != nil || !holds {
		return err
	}
	return stampChapterBasis(ctx, tx, ref)
}

// withoutOwnOffered is a with each offered identifier the book at ref already
// holds of its own left out (MatchAnchor): its value now, before the edit, the
// same, from any layer but a community override. a itself is not changed.
func withoutOwnOffered(ctx context.Context, tx *sql.Tx, ref Ref, a *MatchAnchor) (*MatchAnchor, error) {
	if a.OfferedASIN == "" && a.OfferedISBN == "" {
		return a, nil
	}
	id, err := bookIDByPath(ctx, tx, ref.LibraryID, ref.Path)
	if errors.Is(err, ErrNotFound) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	l, err := loadLayers(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	fields := l.resolve()
	out := *a
	for field, offered := range map[string]*string{FieldASIN: &out.OfferedASIN, FieldISBN: &out.OfferedISBN} {
		fv := fields[field]
		if *offered != "" && !(fv.Locked && fv.Source == SourceCommunity) &&
			CleanOverride(field, fv.Value) == *offered {
			*offered = ""
		}
	}
	return &out, nil
}

// storeAnchor is writeAnchor's write of the book_matches row. A write with was
// naming a match (a run's bookkeeping) changes only the work, recording and
// offered identifiers, keeping the match's provenance (MatchAnchor; a heal's
// time aside).
func storeAnchor(ctx context.Context, tx *sql.Tx, ref Ref, a, was *MatchAnchor, editor any, now string) error {
	if was != nil && was.WorkID != "" {
		// A heal to another work id (a merged work's) stamps the time alone:
		// the upcoming job reads a match's change of work by it (set_at).
		res, err := tx.ExecContext(ctx,
			`UPDATE book_matches SET work_id = ?3, recording_id = ?4, offered_asin = ?5, offered_isbn = ?6,
			        set_at = CASE WHEN work_id = ?3 THEN set_at ELSE ?11 END
			  WHERE library_id = ?1 AND path = ?2 AND work_id = ?7 AND recording_id = ?8
			    AND offered_asin = ?9 AND offered_isbn = ?10`,
			ref.LibraryID, ref.Path, a.WorkID, a.RecordingID, a.OfferedASIN, a.OfferedISBN,
			was.WorkID, was.RecordingID, was.OfferedASIN, was.OfferedISBN, now)
		return anchorWritten(res, err)
	}
	args := []any{ref.LibraryID, ref.Path, a.WorkID, a.RecordingID, a.How, nullableID(a.RunID), editor, now,
		a.OfferedASIN, a.OfferedISBN}
	upsert := ` ON CONFLICT(library_id, path) DO UPDATE SET
		     work_id = excluded.work_id, recording_id = excluded.recording_id, how = excluded.how,
		     run_id = excluded.run_id, set_by = excluded.set_by, set_at = excluded.set_at,
		     offered_asin = excluded.offered_asin, offered_isbn = excluded.offered_isbn`
	if was != nil { // none recorded, as the plan read it: a match made since stays
		upsert = ` ON CONFLICT(library_id, path) DO NOTHING`
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, run_id, set_by, set_at,
		                          offered_asin, offered_isbn)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`+upsert, args...)
	if was == nil {
		return err
	}
	return anchorWritten(res, err)
}

// anchorWritten is a conditional anchor write's answer: ErrMatchedSince when it
// touched no row (the recorded match is no longer the one read).
func anchorWritten(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMatchedSince
	}
	return nil
}

// BookMatchAt is the recorded community match of the book at (libraryID,
// path), nil when it has none (/meta's and /next's work when it names another
// than the book's identifiers do, or they match nothing: api.bookEnrichment).
func (c *Catalog) BookMatchAt(ctx context.Context, libraryID int64, path string) (*BookMatch, error) {
	return bookAnchor(ctx, c.db, libraryID, path)
}

// WorkSource is where a book's work key (Book.WorkID, workIDExpr) comes from:
// its recorded match's work (book_matches, MatchWorkID), which wins whatever
// its identifiers say, else the upcoming job's resolution of those identifiers,
// checked at ResolvedAt (upcoming_owned.checked_at, a catalog stamp).
type WorkSource struct {
	MatchWorkID string
	ResolvedAt  string
}

// WorkSources is each of refs' WorkSource, only for the indexed books with a
// work key: what tells a match from a resolution (and how old that is) for a
// reader that weighs the key against the metadata cache (the Bridge's
// estimate and book facts).
func (c *Catalog) WorkSources(ctx context.Context, refs []Ref) (map[Ref]WorkSource, error) {
	out := map[Ref]WorkSource{}
	type row struct {
		path string
		WorkSource
	}
	_, byLib := GroupRefs(refs)
	for lib, paths := range byLib {
		for chunk := range slices.Chunk(paths, 500) {
			rows, err := queryRows(ctx, c.db, func(r *sql.Rows, m *row) error { return r.Scan(&m.path, &m.MatchWorkID, &m.ResolvedAt) },
				`SELECT path, matched, resolved FROM (
				   SELECT b.rel_path AS path, COALESCE(`+matchWorkIDExpr("b")+`, '') AS matched,
				          COALESCE(`+ownedExpr("b", "checked_at")+`, '') AS resolved
				     FROM books b WHERE b.library_id = ? AND b.rel_path IN (`+placeholders(len(chunk))+`))
				  WHERE matched <> '' OR resolved <> ''`,
				slices.Concat([]any{lib}, anyOf(chunk))...)
			if err != nil {
				return nil, err
			}
			for _, m := range rows {
				out[Ref{LibraryID: lib, Path: m.path}] = m.WorkSource
			}
		}
	}
	return out, nil
}

// MatchWorkIDs is each of refs' recorded match's work (WorkSource.MatchWorkID),
// only for the indexed books with one: WorkSources without the resolution, for
// a reader that only tells a match from a resolution (/meta's placement,
// meta.LocalBook.Matched).
func (c *Catalog) MatchWorkIDs(ctx context.Context, refs []Ref) (map[Ref]string, error) {
	out := map[Ref]string{}
	type row struct{ path, work string }
	_, byLib := GroupRefs(refs)
	for lib, paths := range byLib {
		for chunk := range slices.Chunk(paths, 500) {
			rows, err := queryRows(ctx, c.db, func(r *sql.Rows, m *row) error { return r.Scan(&m.path, &m.work) },
				`SELECT path, matched FROM (
				   SELECT b.rel_path AS path, `+matchWorkIDExpr("b")+` AS matched
				     FROM books b WHERE b.library_id = ? AND b.rel_path IN (`+placeholders(len(chunk))+`))
				  WHERE matched IS NOT NULL`,
				slices.Concat([]any{lib}, anyOf(chunk))...)
			if err != nil {
				return nil, err
			}
			for _, m := range rows {
				out[Ref{LibraryID: lib, Path: m.path}] = m.work
			}
		}
	}
	return out, nil
}

// bookAnchor reads the anchor at (libraryID, path), nil when it has none.
func bookAnchor(ctx context.Context, q querier, libraryID int64, path string) (*BookMatch, error) {
	var m BookMatch
	err := q.QueryRowContext(ctx, `SELECT work_id, recording_id, how, set_at FROM book_matches
	     WHERE library_id = ? AND path = ?`, libraryID, path).Scan(&m.WorkID, &m.RecordingID, &m.How, &m.MatchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
