package catalog

import (
	"context"
	"database/sql"
	"errors"
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
// offered (the admin unticked it, or reverted it since; server#330).
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

// writeAnchor stores the book's anchor at ref (a non-empty WorkID; an unlink is
// clearCommunity's) in the edit's transaction.
func writeAnchor(ctx context.Context, tx *sql.Tx, ref Ref, a *MatchAnchor, editor any, now string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, run_id, set_by, set_at,
		                          offered_asin, offered_isbn)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     work_id = excluded.work_id, recording_id = excluded.recording_id, how = excluded.how,
		     run_id = excluded.run_id, set_by = excluded.set_by, set_at = excluded.set_at,
		     offered_asin = excluded.offered_asin, offered_isbn = excluded.offered_isbn`,
		ref.LibraryID, ref.Path, a.WorkID, a.RecordingID, a.How, nullableID(a.RunID), editor, now,
		a.OfferedASIN, a.OfferedISBN)
	return err
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
