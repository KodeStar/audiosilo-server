package catalog

import (
	"context"
	"database/sql"
	"slices"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The daily report of unknown Audible ASINs (LibraForge C3, internal/coverage,
// migration 0068): which ASINs it may send, and its memory of what it looked up
// and sent. Nothing here names a book or a path.

// UnknownASINCandidates is every ASIN the report may look up, upper-cased,
// valid (match.ValidASIN), distinct and sorted, from books libraries only:
//   - a book's own ASIN (books.asin: an admin's edit, a metadata.json, the
//     Bridge's backfill or enrichment), except one a community match gave a
//     book (a book_overrides row of source community): the community knows it;
//   - else a book's tag ASIN (books.tag_asin), only when the book has no ASIN or
//     ISBN of its own (matchrun.QueryFor's rule for sending the hint), no admin
//     edit of its ASIN (a cleared one too, as BackfillASIN's rule) and that tag
//     is on exactly one book (a tag a batch of books share is a mis-tag).
func (c *Catalog) UnknownASINCandidates(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT upper(trim(b.asin)) FROM books b WHERE `+inBookLibrary+` AND trim(b.asin) <> ''
		UNION
		SELECT upper(trim(b.tag_asin)) FROM books b
		 WHERE `+inBookLibrary+` AND trim(b.tag_asin) <> '' AND b.asin = '' AND b.isbn = ''
		   AND NOT EXISTS(SELECT 1 FROM book_overrides o WHERE o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?1)
		   AND upper(trim(b.tag_asin)) IN (SELECT upper(trim(t.tag_asin)) FROM books t
		        WHERE t.`+bookLibraryIDs+` AND trim(t.tag_asin) <> ''
		        GROUP BY upper(trim(t.tag_asin)) HAVING COUNT(*) = 1)
		EXCEPT
		SELECT upper(trim(b.asin)) FROM books b JOIN book_overrides o
		    ON o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?1 AND o.source = ?2
		 WHERE `+inBookLibrary+` AND trim(b.asin) <> ''`, FieldASIN, SourceCommunity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		if a = match.NormalizeASIN(a); match.ValidASIN(a) {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// UnknownASINRow is what the report remembers of one ASIN: when it last looked
// it up (CheckedAt), whether the answer was "unknown", and when it last sent it
// ("" for never). Times are fixed-width UTC milliseconds.
type UnknownASINRow struct {
	CheckedAt string
	Unknown   bool
	SentAt    string
}

// UnknownASINRows returns every remembered ASIN, by ASIN.
func (c *Catalog) UnknownASINRows(ctx context.Context) (map[string]UnknownASINRow, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT asin, checked_at, unknown, COALESCE(sent_at, '') FROM unknown_asin_reports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]UnknownASINRow{}
	for rows.Next() {
		var asin string
		var r UnknownASINRow
		if err := rows.Scan(&asin, &r.CheckedAt, &r.Unknown, &r.SentAt); err != nil {
			return nil, err
		}
		out[asin] = r
	}
	return out, rows.Err()
}

// DropUnknownASINs forgets asins (no book holds them any more).
func (c *Catalog) DropUnknownASINs(ctx context.Context, asins []string) error {
	if len(asins) == 0 {
		return nil
	}
	return c.db.WithUserStateTx(ctx, "DropUnknownASINs", func(tx *sql.Tx) error {
		for _, a := range asins {
			if _, err := tx.ExecContext(ctx, `DELETE FROM unknown_asin_reports WHERE asin = ?`, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// ASINCheck is one lookup's answer: Unknown when the community database has no
// recording with the ASIN.
type ASINCheck struct {
	ASIN    string
	Unknown bool
}

// SaveASINChecks records lookups made at at. A known ASIN forgets when it was
// sent (it is never sent again while known); an unknown one keeps it, so it is
// sent again only once the resend age has passed.
func (c *Catalog) SaveASINChecks(ctx context.Context, checks []ASINCheck, at string) error {
	if len(checks) == 0 {
		return nil
	}
	return c.db.WithUserStateTx(ctx, "SaveASINChecks", func(tx *sql.Tx) error {
		for _, ch := range checks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO unknown_asin_reports(asin, checked_at, unknown, sent_at)
				VALUES(?1, ?2, ?3, NULL)
				ON CONFLICT(asin) DO UPDATE SET checked_at = excluded.checked_at, unknown = excluded.unknown,
				  sent_at = CASE WHEN excluded.unknown = 1 THEN unknown_asin_reports.sent_at END`,
				ch.ASIN, at, ch.Unknown); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkASINsSent records that the metadata service accepted asins at at.
func (c *Catalog) MarkASINsSent(ctx context.Context, asins []string, at string) error {
	if len(asins) == 0 {
		return nil
	}
	return c.db.WithUserStateTx(ctx, "MarkASINsSent", func(tx *sql.Tx) error {
		for _, a := range asins {
			if _, err := tx.ExecContext(ctx, `UPDATE unknown_asin_reports SET sent_at = ? WHERE asin = ?`, at, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// LastASINSentAt is when the report last sent anything ("" for never):
// MAX(sent_at), which keeps it to once a day across restarts.
func (c *Catalog) LastASINSentAt(ctx context.Context) (string, error) {
	var at sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT MAX(sent_at) FROM unknown_asin_reports`).Scan(&at)
	return at.String, err
}

// unknownASINReportKey is the report's run status row in server_state.
const unknownASINReportKey = "unknown_asin_report"

// UnknownASINReport is the report's run status (server_state
// 'unknown_asin_report'). Times are fixed-width UTC milliseconds, "" for never.
type UnknownASINReport struct {
	// State is the last run's outcome: "" before any, else ok, not_offered or
	// failed (coverage's State* constants).
	State     string `json:"state,omitempty"`
	LastRunAt string `json:"last_run_at,omitempty"`
	// LastSent is how many ASINs the last run's accepted reports held.
	LastSent int `json:"last_sent"`
	// QuietUntil is when the report is tried again after the service said it
	// takes none (not_offered); "" when it isn't waiting.
	QuietUntil string `json:"quiet_until,omitempty"`
}

// UnknownASINReportState reads the report's run status (the zero value before
// its first run).
func (c *Catalog) UnknownASINReportState(ctx context.Context) (UnknownASINReport, error) {
	return getServerState[UnknownASINReport](ctx, c.db, unknownASINReportKey)
}

// SaveUnknownASINReportState records the report's run status.
func (c *Catalog) SaveUnknownASINReportState(ctx context.Context, r UnknownASINReport) error {
	return putServerState(ctx, c.db, unknownASINReportKey, r, c.stamp())
}
