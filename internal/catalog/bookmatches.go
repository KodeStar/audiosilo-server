package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
// sets one.
type MatchAnchor struct {
	WorkID      string
	RecordingID string
	How         string
	RunID       int64
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

// normalizeAnchor validates an edit's anchor (nil = none) and returns it in stored
// form. Setting one takes a community edit (source, normalized); removing one
// (an empty WorkID) any edit.
func normalizeAnchor(a *MatchAnchor, source string) (*MatchAnchor, error) {
	if a == nil {
		return nil, nil
	}
	out := MatchAnchor{WorkID: strings.TrimSpace(a.WorkID), RecordingID: strings.TrimSpace(a.RecordingID), How: a.How, RunID: a.RunID}
	if out.WorkID == "" {
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
		`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, run_id, set_by, set_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     work_id = excluded.work_id, recording_id = excluded.recording_id, how = excluded.how,
		     run_id = excluded.run_id, set_by = excluded.set_by, set_at = excluded.set_at`,
		ref.LibraryID, ref.Path, a.WorkID, a.RecordingID, a.How, nullableID(a.RunID), editor, now)
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
