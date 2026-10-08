package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Listening imports (internal/importer fetches, matches and plans them): an
// imports row per (source user, AudioSilo user), and the rows an applied import
// wrote, each marked with its id (migration 0034). This file is the data layer:
// the rows, the one transaction that applies an import (after taking back the
// person's previous one from the same source) and the one that undoes it. What to
// write is the importer's (ImportWrite), worked out from the state ImportState
// reads.

// Import statuses.
const (
	ImportFetching = "fetching" // reading the source's history in the background
	ImportReview   = "review"   // fetched and planned; waiting for the admin
	ImportApplying = "applying"
	ImportApplied  = "applied"
	ImportFailed   = "failed"
	ImportUndone   = "undone"
)

// ImportSourceABS is an import from Audiobookshelf (the only source in v1).
const ImportSourceABS = "abs"

// Why an import's item was not matched (UnmatchedItem.Reason).
const (
	ReasonNoMatch  = "no_match"
	ReasonContest  = "contested"   // two source items resolved to one book: both skipped
	ReasonNoAccess = "no_access"   // the book is here, but the person can't see it
	ReasonSplit    = "split_discs" // a book split across disc folders here: join the discs first
)

// Import errors.
var (
	// ErrImportRunning: the person already has an import fetching or applying.
	ErrImportRunning = errors.New("the user already has an import in progress")
	// ErrImportNotReady: a review step (cutoff change, apply) asked of an import
	// that isn't waiting for review.
	ErrImportNotReady = errors.New("the import is not ready")
	// ErrImportNotApplied: an undo of an import that isn't applied.
	ErrImportNotApplied = errors.New("the import is not applied")
	// ErrImportApplied: a delete of an applied import (undo it first).
	ErrImportApplied = errors.New("the import is applied")
)

// ImportMatched counts an import's matched items by how they matched.
type ImportMatched struct {
	Path  int `json:"path"`
	ASIN  int `json:"asin"`
	ISBN  int `json:"isbn"`
	Title int `json:"title"`
}

// ImportSummary is what an import adds (planned, or applied). Seconds for the
// listening; first/last listen are RFC3339 UTC, null without sessions.
type ImportSummary struct {
	Items              int           `json:"items"`
	Matched            ImportMatched `json:"matched"`
	Unmatched          int           `json:"unmatched"`
	Sessions           int           `json:"sessions"`
	SkippedAfterCutoff int           `json:"skipped_after_cutoff"`
	Listened           float64       `json:"listened"`
	Estimated          float64       `json:"estimated"`
	Progress           int           `json:"progress"`
	Finished           int           `json:"finished"`
	Bookmarks          int           `json:"bookmarks"`
	FirstListen        *string       `json:"first_listen"`
	LastListen         *string       `json:"last_listen"`
}

// UnmatchedItem is a source book an import skips, with the listening it had.
type UnmatchedItem struct {
	Title    string  `json:"title"`
	Author   string  `json:"author"`
	Listened float64 `json:"listened"`
	Sessions int     `json:"sessions"`
	Reason   string  `json:"reason"`
}

// Import is one import as the admin console lists it. Cutoff, CreatedAt and
// AppliedAt are RFC3339 UTC. CutoffOffset is the server's offset from UTC at the
// cutoff (minutes; nil without one), so the console reads the cutoff as the
// server's day and time: a chosen day is its start in server time. Error is a
// safe sentence and ErrorCode a code, both "" unless it failed. Summary is nil
// until the history is fetched and planned.
type Import struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Username   string  `json:"username"`
	Source     string  `json:"source"`
	SourceURL  string  `json:"source_url"`
	SourceUser string  `json:"source_user"`
	SourceID   string  `json:"-"` // the source's id for the user
	Status     string  `json:"status"`
	Cutoff     *string `json:"cutoff"`
	// CutoffOffset is filled from Cutoff as the import is read (cutoffOffset).
	CutoffOffset *int           `json:"cutoff_utc_offset"`
	CreatedAt    string         `json:"created_at"`
	AppliedAt    *string        `json:"applied_at"`
	Error        string         `json:"error"`
	ErrorCode    string         `json:"error_code"`
	Summary      *ImportSummary `json:"summary"`
}

// ImportDetail is an import with the items it skips (most listened first).
type ImportDetail struct {
	Import
	UnmatchedItems []UnmatchedItem `json:"unmatched_items"`
}

// CutoffTime is an import's cutoff as a time (zero when it has none).
func (i Import) CutoffTime() time.Time {
	if i.Cutoff == nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, *i.Cutoff)
	return t
}

const importColumns = `i.id, i.user_id, u.username, i.source, i.source_url, i.source_user, i.source_id, i.status,
	i.cutoff, i.created_at, i.applied_at, i.summary, i.error, i.error_code`

const importFrom = ` FROM imports i JOIN users u ON u.id = i.user_id`

func scanImport(row interface{ Scan(...any) error }, extra ...any) (Import, error) {
	var (
		imp     Import
		summary sql.NullString
	)
	if err := row.Scan(append([]any{&imp.ID, &imp.UserID, &imp.Username, &imp.Source, &imp.SourceURL,
		&imp.SourceUser, &imp.SourceID, &imp.Status, &imp.Cutoff, &imp.CreatedAt, &imp.AppliedAt, &summary,
		&imp.Error, &imp.ErrorCode}, extra...)...); err != nil {
		return imp, err
	}
	imp.CutoffOffset = cutoffOffset(imp.CutoffTime(), time.Local)
	if summary.Valid {
		imp.Summary = &ImportSummary{}
		if err := json.Unmarshal([]byte(summary.String), imp.Summary); err != nil {
			return imp, err
		}
	}
	return imp, nil
}

// cutoffOffset is loc's offset from UTC at cutoff, in minutes (nil for no
// cutoff). loc is the server's zone: time.Local, where a cutoff day starts
// (the API reads a YYYY-MM-DD cutoff in it) and the importer counts days.
func cutoffOffset(cutoff time.Time, loc *time.Location) *int {
	if cutoff.IsZero() {
		return nil
	}
	_, secs := cutoff.In(loc).Zone()
	m := secs / 60
	return &m
}

// FormatStamp is t in the fixed-width UTC millisecond form sessions, bookmarks
// and a server-stamped progress updated_at are stored in (they compare as text).
func FormatStamp(t time.Time) string { return formatSessionTime(t) }

// FormatCutoff is a cutoff as imports.cutoff stores it (RFC3339 UTC, to the
// second, rounded down so a session in the cutoff's own second stays out).
func FormatCutoff(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Truncate(time.Second).Format(time.RFC3339)
	return &s
}

// CreateImports records new imports (status fetching), all or none, and returns
// them. ErrImportRunning when one of their users already has an import fetching
// or applying.
func (c *Catalog) CreateImports(ctx context.Context, imps []Import) ([]Import, error) {
	now := c.ts()
	var out []Import
	err := c.db.WithTx(ctx, "CreateImports", func(tx *sql.Tx) error {
		ids := make([]any, 0, len(imps))
		for _, imp := range imps {
			var busy bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM imports WHERE user_id = ? AND status IN (?, ?))`,
				imp.UserID, ImportFetching, ImportApplying).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return ErrImportRunning
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO imports(user_id, source, source_url, source_user, source_id, status, cutoff, created_at)
				 VALUES(?,?,?,?,?,?,?,?)`,
				imp.UserID, imp.Source, imp.SourceURL, imp.SourceUser, imp.SourceID, ImportFetching, imp.Cutoff, now)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		var err error
		out, err = queryRows(ctx, tx, func(rows *sql.Rows, imp *Import) error {
			var err error
			*imp, err = scanImport(rows)
			return err
		}, `SELECT `+importColumns+importFrom+` WHERE i.id IN (`+placeholders(len(ids))+`) ORDER BY i.id`, ids...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetImport returns one import with its unmatched items, or ErrNotFound.
func (c *Catalog) GetImport(ctx context.Context, id int64) (*ImportDetail, error) {
	var unmatched string
	imp, err := scanImport(c.db.QueryRowContext(ctx,
		`SELECT `+importColumns+`, i.unmatched`+importFrom+` WHERE i.id = ?`, id), &unmatched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := &ImportDetail{Import: imp, UnmatchedItems: []UnmatchedItem{}}
	if err := json.Unmarshal([]byte(unmatched), &d.UnmatchedItems); err != nil {
		return nil, err
	}
	return d, nil
}

// getImport reads import id through q, without its unmatched items; ErrNotFound.
func getImport(ctx context.Context, q querier, id int64) (Import, error) {
	imp, err := scanImport(q.QueryRowContext(ctx, `SELECT `+importColumns+importFrom+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return imp, ErrNotFound
	}
	return imp, err
}

// ListImports returns the imports of one user (0 = everyone's), newest first.
func (c *Catalog) ListImports(ctx context.Context, userID int64) ([]Import, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, imp *Import) error {
		var err error
		*imp, err = scanImport(rows)
		return err
	}, `SELECT `+importColumns+importFrom+` WHERE ?1 = 0 OR i.user_id = ?1 ORDER BY i.id DESC`, userID)
}

// ImportPayload returns an import's fetched history as the importer stored it:
// nil before the fetch finished and once the import is applied, undone or gone.
func (c *Catalog) ImportPayload(ctx context.Context, id int64) ([]byte, error) {
	var b []byte
	err := c.db.QueryRowContext(ctx, `SELECT payload FROM import_payloads WHERE import_id = ?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ImportPlan is what a review shows: the summary and the skipped items.
type ImportPlan struct {
	Summary   ImportSummary
	Unmatched []UnmatchedItem
}

func (p ImportPlan) encode() (summary, unmatched string, err error) {
	s, err := json.Marshal(p.Summary)
	if err != nil {
		return "", "", err
	}
	items := p.Unmatched
	if items == nil {
		items = []UnmatchedItem{}
	}
	u, err := json.Marshal(items)
	return string(s), string(u), err
}

// FinishImportFetch stores a fetched history and its plan, moving the import to
// review. A no-op for an import no longer fetching (deleted meanwhile).
func (c *Catalog) FinishImportFetch(ctx context.Context, id int64, sourceUser string, payload []byte, p ImportPlan) error {
	summary, unmatched, err := p.encode()
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "FinishImportFetch", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE imports SET status = ?, source_user = ?, summary = ?, unmatched = ? WHERE id = ? AND status = ?`,
			ImportReview, sourceUser, summary, unmatched, id, ImportFetching)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO import_payloads(import_id, payload) VALUES(?, ?)`, id, payload)
		return err
	})
}

// FailImport marks a fetching import failed with a code and a safe sentence.
func (c *Catalog) FailImport(ctx context.Context, id int64, code, msg string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE imports SET status = ?, error_code = ?, error = ? WHERE id = ? AND status = ?`,
		ImportFailed, code, msg, id, ImportFetching)
	return err
}

// SetImportReview changes a reviewed import's cutoff and stores the plan worked
// out for it. ErrNotFound, or ErrImportNotReady unless it is in review.
func (c *Catalog) SetImportReview(ctx context.Context, id int64, cutoff *string, p ImportPlan) error {
	summary, unmatched, err := p.encode()
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "SetImportReview", func(tx *sql.Tx) error {
		if err := importInStatus(ctx, tx, id, ImportReview, ErrImportNotReady); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE imports SET cutoff = ?, summary = ?, unmatched = ? WHERE id = ?`,
			cutoff, summary, unmatched, id)
		return err
	})
}

// importStatus is import id's status; ErrNotFound without the row.
func importStatus(ctx context.Context, q querier, id int64) (string, error) {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM imports WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// importInStatus checks import id's status inside tx: ErrNotFound without the
// row, wrong when it is in another status.
func importInStatus(ctx context.Context, tx *sql.Tx, id int64, status string, wrong error) error {
	have, err := importStatus(ctx, tx, id)
	if err != nil {
		return err
	}
	if have != status {
		return wrong
	}
	return nil
}

// DeleteImport removes an import that isn't applied (its rows were never written,
// or an undo took them out). ErrNotFound; ErrImportApplied when applied,
// ErrImportNotReady while it is being applied.
func (c *Catalog) DeleteImport(ctx context.Context, id int64) error {
	return c.db.WithTx(ctx, "DeleteImport", func(tx *sql.Tx) error {
		status, err := importStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		switch status {
		case ImportApplied:
			return ErrImportApplied
		case ImportApplying:
			return ErrImportNotReady
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM imports WHERE id = ?`, id)
		return err
	})
}

// ImportInterrupted is the error code of a fetch the server stopped during.
const ImportInterrupted = "interrupted"

// InterruptImports settles the imports a stopped server left (at startup): a
// fetch fails (its token is gone with the process), an apply goes back to review
// (it is one transaction, so nothing of it was written).
func (c *Catalog) InterruptImports(ctx context.Context) error {
	return c.db.WithTx(ctx, "InterruptImports", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE imports SET status = ?, error_code = ?, error = ? WHERE status = ?`,
			ImportFailed, ImportInterrupted, "The server stopped while this import was fetching. Start it again.",
			ImportFetching); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE imports SET status = ? WHERE status = ?`, ImportReview, ImportApplying)
		return err
	})
}

// ListeningStart is when userID's listening recorded here (not imported) began:
// their first session with listening, or the start of the first rolled-up day
// (in loc, the zone listening_daily's days are in), whichever is earlier. Zero
// when there is none. Estimated days are left out: an estimate has no real day.
func (c *Catalog) ListeningStart(ctx context.Context, userID int64, loc *time.Location) (time.Time, error) {
	var first, day sql.NullString
	if err := c.db.QueryRowContext(ctx,
		`SELECT (SELECT MIN(started_at) FROM listening_sessions
		          WHERE user_id = ?1 AND import_id = 0 AND listened > 0),
		        (SELECT MIN(day) FROM listening_daily
		          WHERE user_id = ?1 AND import_id = 0 AND estimated = 0 AND listened > 0)`,
		userID).Scan(&first, &day); err != nil {
		return time.Time{}, err
	}
	var out time.Time
	if first.Valid {
		out = parseSessionTime(first.String)
	}
	if day.Valid {
		if d, err := time.ParseInLocation(time.DateOnly, day.String, loc); err == nil && (out.IsZero() || d.Before(out)) {
			out = d
		}
	}
	return out, nil
}

// ImportBooks returns every indexed book with what the importer matches on.
func (c *Catalog) ImportBooks(ctx context.Context) ([]Book, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, b *Book) error {
		return rows.Scan(&b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author, &b.Series, &b.SeriesIndex,
			&b.ASIN, &b.ISBN, &b.Duration, &b.Codec, &b.SplitParent)
	}, `SELECT b.library_id, b.rel_path, b.is_folder, b.title, b.author, b.series, b.series_index, b.asin, b.isbn,
	           b.duration, b.codec, b.split_parent
	      FROM books b JOIN libraries l ON l.id = b.library_id
	     ORDER BY l.sort_order, l.id, b.rel_path`)
}

// ImportState is what an import is planned against: the import, and the
// person's progress and bookmarks as they will be once their previous import from
// the same source is taken back (an apply does that first).
type ImportState struct {
	Import    Import
	Progress  map[Ref]Progress
	Bookmarks map[Ref][]Bookmark
}

// ImportStateFor reads import id's ImportState, for a review.
func (c *Catalog) ImportStateFor(ctx context.Context, id int64) (*ImportState, error) {
	imp, err := getImport(ctx, c.db, id)
	if err != nil {
		return nil, err
	}
	return importState(ctx, c.db, imp)
}

// prevApplied is the id of the person's applied import from imp's source other
// than imp (0: none). An apply takes the previous one back first, so there is at
// most one.
func prevApplied(ctx context.Context, q querier, imp Import) (int64, error) {
	var prev int64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM imports WHERE user_id = ? AND source = ? AND status = ? AND id <> ?`,
		imp.UserID, imp.Source, ImportApplied, imp.ID).Scan(&prev)
	return prev, err
}

// importState reads imp's ImportState through q. The person's applied import
// from imp's source (another one) is undone in what it reads, exactly as
// ApplyImport's undo of it leaves things: the bookmarks it would delete left out,
// the progress rows it wrote and nobody changed since read as restoredProgress
// restores them.
func importState(ctx context.Context, q querier, imp Import) (*ImportState, error) {
	st := &ImportState{Import: imp, Progress: map[Ref]Progress{}, Bookmarks: map[Ref][]Bookmark{}}
	prev, err := prevApplied(ctx, q, imp)
	if err != nil {
		return nil, err
	}
	rows, err := queryRows(ctx, q, func(rows *sql.Rows, p *Progress) error { return scanProgress(rows, p) },
		`SELECT `+progressColumns+` FROM progress WHERE user_id = ?`, imp.UserID)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		st.Progress[p.Ref] = p
	}
	if prev != 0 {
		priors, err := unchangedPriors(ctx, q, prev)
		if err != nil {
			return nil, err
		}
		for _, pr := range priors {
			if pr.prior == nil {
				delete(st.Progress, pr.ref)
			} else {
				st.Progress[pr.ref] = pr.restored()
			}
		}
	}
	marks, err := queryRows(ctx, q, func(rows *sql.Rows, b *Bookmark) error { return scanBookmark(rows, b) },
		`SELECT `+bookmarkColumns+` FROM bookmarks
		  WHERE user_id = ?1 AND NOT (?2 <> 0 AND import_id = ?2 AND `+bookmarkUntouched+`)`,
		imp.UserID, prev)
	if err != nil {
		return nil, err
	}
	for _, b := range marks {
		st.Bookmarks[b.Ref] = append(st.Bookmarks[b.Ref], b)
	}
	return st, nil
}

// ImportSession is a listening session an import writes (times are UTC).
type ImportSession struct {
	Ref
	DeviceName    string
	ClientVersion string
	Platform      string
	StartedAt     time.Time
	LastAt        time.Time
	StartPos      float64
	EndPos        float64
	Duration      float64
	Listened      float64
}

// ImportDay is a listening_daily row an import writes (an estimate).
type ImportDay struct {
	Ref
	Day      string // YYYY-MM-DD, server time
	Listened float64
}

// ImportProgress is a progress row an import writes: Prior is the row it
// replaces (nil: there was none), Wrote the row it writes.
type ImportProgress struct {
	Prior *Progress
	Wrote Progress
}

// ImportWrite is everything an apply writes, and what the import then reports.
type ImportWrite struct {
	Sessions  []ImportSession // oldest first
	Days      []ImportDay
	Bookmarks []Bookmark
	Progress  []ImportProgress
	Plan      ImportPlan
	// RollUpBefore is the session retention's cutoff, as PruneSessions is given
	// it (zero: none). A session whose last save is before it is written as the
	// listening_daily rows retention would sum it into (per day in Zone; nil is
	// time.Local), not as a session the next prune rolls up anyway. Its
	// listening_history span is written either way.
	RollUpBefore time.Time
	Zone         *time.Location
}

// rollUp splits w's sessions for imp into the ones written as sessions and the
// day totals of the ones already past the retention (RollUpBefore), summed as
// PruneSessions sums them.
func (w *ImportWrite) rollUp(imp Import) ([]ImportSession, daySums) {
	if w.RollUpBefore.IsZero() {
		return w.Sessions, nil
	}
	loc := cmp.Or(w.Zone, time.Local)
	cutoff := formatSessionTime(w.RollUpBefore)
	raw, sums := make([]ImportSession, 0, len(w.Sessions)), daySums{}
	for _, s := range w.Sessions {
		if formatSessionTime(s.LastAt) >= cutoff { // PruneSessions' last_at < cutoff, inverted
			raw = append(raw, s)
			continue
		}
		sums.add(dayKey{user: imp.UserID, lib: s.LibraryID, path: s.Path, importID: imp.ID},
			s.StartedAt, s.LastAt, s.Listened, loc)
	}
	return raw, sums
}

// ImportClientApp is the app an imported session names.
const ImportClientApp = "Audiobookshelf"

// ApplyImport applies import id, which must be in review: it moves to applying,
// then one transaction takes back the person's previous applied import from the
// same source (it becomes undone), reads the ImportState, asks build what to
// write, writes it and marks the import applied. A failure puts it back in
// review. ErrNotFound, or ErrImportNotReady unless it was in review.
//
// The previous import's progress rows go back exactly as they were before it
// (restoredProgress: their own updated_at), so the new import merges into the
// same state its review planned against (importState) and moves them on again
// as a first import would. A row it then leaves behind where the previous one
// had put it is stamped now, as a standalone undo stamps it (keepRewound).
func (c *Catalog) ApplyImport(ctx context.Context, id int64, build func(ImportState) (*ImportWrite, error)) (*Import, error) {
	res, err := c.db.ExecContext(ctx, `UPDATE imports SET status = ? WHERE id = ? AND status = ?`,
		ImportApplying, id, ImportReview)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := importStatus(ctx, c.db, id); err != nil {
			return nil, err
		}
		return nil, ErrImportNotReady
	}
	err = c.db.WithTx(ctx, "ApplyImport", func(tx *sql.Tx) error {
		imp, err := getImport(ctx, tx, id)
		if err != nil {
			return err
		}
		prev, err := prevApplied(ctx, tx, imp)
		if err != nil {
			return err
		}
		var rewound []progressPrior
		if prev != 0 {
			if rewound, err = c.undoImport(ctx, tx, prev, true); err != nil {
				return err
			}
		}
		st, err := importState(ctx, tx, imp)
		if err != nil {
			return err
		}
		w, err := build(*st)
		if err != nil {
			return err
		}
		if err := c.keepRewound(ctx, tx, imp.UserID, rewound, w); err != nil {
			return err
		}
		if err := c.writeImport(ctx, tx, imp, w); err != nil {
			return err
		}
		summary, unmatched, err := w.Plan.encode()
		if err != nil {
			return err
		}
		// Nothing reads the fetched history once the import is applied.
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_payloads WHERE import_id = ?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE imports SET status = ?, applied_at = ?, summary = ?, unmatched = ? WHERE id = ?`,
			ImportApplied, c.ts(), summary, unmatched, id)
		return err
	})
	if err != nil {
		if _, rerr := c.db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE imports SET status = ? WHERE id = ? AND status = ?`, ImportReview, id, ImportApplying); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return nil, err
	}
	imp, err := getImport(ctx, c.db, id)
	if err != nil {
		return nil, err
	}
	return &imp, nil
}

// writeImport writes w for imp inside tx, each kind of row through one prepared
// statement.
func (c *Catalog) writeImport(ctx context.Context, tx *sql.Tx, imp Import, w *ImportWrite) error {
	raw, rolled := w.rollUp(imp)
	if err := execEach(ctx, tx,
		`INSERT INTO listening_sessions(user_id, library_id, rel_path, device_name, client_app, client_version,
		     client_platform, started_at, last_at, start_pos, end_pos, duration, speed, listened, backfilled, import_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,?,1,?)`, len(raw), func(i int) ([]any, error) {
			s := raw[i]
			return []any{imp.UserID, s.LibraryID, s.Path, s.DeviceName, ImportClientApp, s.ClientVersion, s.Platform,
				formatSessionTime(s.StartedAt), formatSessionTime(s.LastAt), s.StartPos, s.EndPos, s.Duration,
				s.Listened, imp.ID}, nil
		}); err != nil {
		return err
	}
	if err := rolled.insert(ctx, tx); err != nil {
		return err
	}
	if err := execEach(ctx, tx,
		`INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions, estimated, import_id)
		 VALUES(?,?,?,?,?,0,1,?)`, len(w.Days), func(i int) ([]any, error) {
			d := w.Days[i]
			return []any{d.Day, imp.UserID, d.LibraryID, d.Path, d.Listened, imp.ID}, nil
		}); err != nil {
		return err
	}
	if err := execEach(ctx, tx,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, label, created_at, import_id, import_note)
		 VALUES(?,?,?,?,?,'',?,?,?)`, len(w.Bookmarks), func(i int) ([]any, error) {
			b := w.Bookmarks[i]
			return []any{imp.UserID, b.LibraryID, b.Path, b.Position, b.Note, b.CreatedAt, imp.ID, b.Note}, nil
		}); err != nil {
		return err
	}
	// Each session is also a player-style span (the book's History, the
	// Journal read listening_history, not the sessions), a rolled-up one too:
	// history is never pruned.
	if err := execEach(ctx, tx,
		`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at, import_id)
		 VALUES(?,?,?,?,?,?,?,?)`, len(w.Sessions), func(i int) ([]any, error) {
			s := w.Sessions[i]
			return []any{imp.UserID, s.LibraryID, s.Path, s.StartPos, s.EndPos, formatSessionTime(s.StartedAt),
				formatSessionTime(s.LastAt), imp.ID}, nil
		}); err != nil {
		return err
	}
	if err := execEach(ctx, tx, progressUpsert, len(w.Progress), func(i int) ([]any, error) {
		return progressRowArgs(imp.UserID, w.Progress[i].Wrote), nil
	}); err != nil {
		return err
	}
	return execEach(ctx, tx,
		`INSERT INTO import_progress_prior(import_id, user_id, library_id, rel_path, prior, wrote)
		 VALUES(?,?,?,?,?,?)`, len(w.Progress), func(i int) ([]any, error) {
			p := w.Progress[i]
			prior, err := jsonOrNil(p.Prior)
			if err != nil {
				return nil, err
			}
			wrote, err := json.Marshal(p.Wrote)
			return []any{imp.ID, imp.UserID, p.Wrote.LibraryID, p.Wrote.Path, prior, string(wrote)}, err
		})
}

// execEach runs query once per row (n of them, args giving each one's values)
// through one statement prepared in tx.
func execEach(ctx context.Context, tx *sql.Tx, query string, n int, args func(i int) ([]any, error)) error {
	if n == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for i := range n {
		a, err := args(i)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, a...); err != nil {
			return err
		}
	}
	return nil
}

func jsonOrNil(p *Progress) (any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// progressUpsert writes a progress row whole (dates "" stored as NULL), with
// progressRowArgs' arguments.
const progressUpsert = `INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished,
	    playback_speed, version, device_id, updated_at, started_at, finished_at)
	VALUES(?,?,?,?,?,?,?,?,?,?,NULLIF(?, ''),NULLIF(?, ''))
	ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
	    position=excluded.position, duration=excluded.duration, finished=excluded.finished,
	    playback_speed=excluded.playback_speed, version=excluded.version, device_id=excluded.device_id,
	    updated_at=excluded.updated_at, started_at=excluded.started_at, finished_at=excluded.finished_at`

// progressRowArgs are progressUpsert's arguments for p as userID's progress.
func progressRowArgs(userID int64, p Progress) []any {
	return []any{userID, p.LibraryID, p.Path, p.Position, p.Duration, p.Finished, p.PlaybackSpeed, p.Version,
		p.DeviceID, p.UpdatedAt, p.StartedAt, p.FinishedAt}
}

// sameProgress reports whether a stored row is still the one an import wrote: a
// player's save or an edit since changes its version or its time, and a move or
// join its path.
func sameProgress(cur, wrote Progress) bool {
	return cur.Ref == wrote.Ref && cur.Version == wrote.Version && cur.UpdatedAt == wrote.UpdatedAt &&
		cur.Position == wrote.Position && cur.Finished == wrote.Finished &&
		cur.StartedAt == wrote.StartedAt && cur.FinishedAt == wrote.FinishedAt
}

// progressPrior is one import_progress_prior row whose progress row is still the
// one the import wrote.
type progressPrior struct {
	ref   Ref
	prior *Progress // nil: there was none
	wrote Progress
}

// restored is the row an undo puts back for pr (prior must be set): the prior
// row as it was, under a version above the one the import wrote.
func (pr progressPrior) restored() Progress {
	back := *pr.prior
	back.Version = pr.wrote.Version + 1
	return back
}

// rewound reports whether the import had moved pr's row on in time (its
// updated_at), so a device may hold the import's row as the newer one.
func (pr progressPrior) rewound() bool {
	return pr.prior != nil && pr.wrote.UpdatedAt != pr.prior.UpdatedAt
}

// bookmarkUntouched (SQL, on a bookmarks row) is an imported bookmark as its
// import wrote it: the same note, no label. An undo deletes only those; one the
// person has edited or labelled since is theirs.
const bookmarkUntouched = `note = import_note AND label = ''`

// prefixScan scans pre's destinations, then the ones a scan function names.
type prefixScan struct {
	rows *sql.Rows
	pre  []any
}

func (p prefixScan) Scan(dest ...any) error { return p.rows.Scan(append(p.pre, dest...)...) }

// unchangedPriors reads import importID's import_progress_prior rows with the
// progress row on each path in one query, keeping those nobody has changed since
// the import wrote them (sameProgress): the ones an undo restores.
func unchangedPriors(ctx context.Context, q querier, importID int64) ([]progressPrior, error) {
	type row struct {
		progressPrior
		cur Progress
	}
	rows, err := queryRows(ctx, q, func(rows *sql.Rows, r *row) error {
		var (
			prior sql.NullString
			wrote string
		)
		if err := scanProgress(prefixScan{rows, []any{&prior, &wrote}}, &r.cur); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(wrote), &r.wrote); err != nil {
			return err
		}
		// The row may have moved with its book since (carryListeningState).
		r.ref, r.wrote.Ref = r.cur.Ref, r.cur.Ref
		if prior.Valid {
			r.prior = &Progress{}
			if err := json.Unmarshal([]byte(prior.String), r.prior); err != nil {
				return err
			}
			r.prior.Ref = r.cur.Ref
		}
		return nil
	}, `SELECT prior, wrote, `+progressColumns+`
	      FROM import_progress_prior JOIN progress USING (user_id, library_id, rel_path)
	     WHERE import_id = ?`, importID)
	if err != nil {
		return nil, err
	}
	out := make([]progressPrior, 0, len(rows))
	for _, r := range rows {
		if sameProgress(r.cur, r.wrote) { // else the listener (or an edit) has moved on: theirs stays
			out = append(out, r.progressPrior)
		}
	}
	return out, nil
}

// UndoImport takes an applied import back out: its sessions, history spans and
// days are deleted, so are its bookmarks unless the person has edited or
// labelled them since (bookmarkUntouched; those are kept as theirs), and every
// progress row it changed that nobody has changed since is restored.
// ErrNotFound, or ErrImportNotApplied unless it is applied.
func (c *Catalog) UndoImport(ctx context.Context, id int64) (*Import, error) {
	err := c.db.WithTx(ctx, "UndoImport", func(tx *sql.Tx) error {
		if err := importInStatus(ctx, tx, id, ImportApplied, ErrImportNotApplied); err != nil {
			return err
		}
		_, err := c.undoImport(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	imp, err := getImport(ctx, c.db, id)
	if err != nil {
		return nil, err
	}
	return &imp, nil
}

// undoImport is UndoImport inside tx (also an apply's taking back of the
// previous import: replacing). A restored row is restoredProgress's. Undone on
// its own, a row the import had moved on (rewound) takes the server's now as
// its updated_at, so a device that synced the imported row takes the restored
// one rather than sending the import's back. Replacing, every row keeps its own
// updated_at, as the new import's review planned against (importState), and the
// rewound ones are returned for ApplyImport to settle once the new import is
// planned (keepRewound).
func (c *Catalog) undoImport(ctx context.Context, tx *sql.Tx, id int64, replacing bool) ([]progressPrior, error) {
	for _, stmt := range []string{
		`DELETE FROM listening_sessions WHERE import_id = ?`,
		`DELETE FROM listening_history WHERE import_id = ?`,
		`DELETE FROM listening_daily WHERE import_id = ?`,
		`DELETE FROM bookmarks WHERE import_id = ? AND ` + bookmarkUntouched,
		// What is left of its bookmarks the person changed: theirs now.
		`UPDATE bookmarks SET import_id = 0, import_note = '' WHERE import_id = ?`,
		`DELETE FROM import_payloads WHERE import_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return nil, err
		}
	}
	var userID int64
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM imports WHERE id = ?`, id).Scan(&userID); err != nil {
		return nil, err
	}
	priors, err := unchangedPriors(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var rewound []progressPrior
	for _, pr := range priors {
		if pr.prior == nil {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
				userID, pr.ref.LibraryID, pr.ref.Path); err != nil {
				return nil, err
			}
			continue
		}
		back := pr.restored()
		if pr.rewound() {
			if replacing {
				rewound = append(rewound, pr)
			} else {
				back.UpdatedAt = formatSessionTime(c.now())
			}
		}
		if _, err := tx.ExecContext(ctx, progressUpsert, progressRowArgs(userID, back)...); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM import_progress_prior WHERE import_id = ?`, id); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE imports SET status = ? WHERE id = ?`, ImportUndone, id)
	return rewound, err
}

// keepRewound settles, inside ApplyImport's tx, the rows the replaced import had
// moved on and its undo put back with their own updated_at (rewound): where the
// new import w doesn't leave the row newer than the replaced import's (IsNewer:
// a device that synced that row would send it back over ours), the row takes
// the server's now as its updated_at, as a standalone undo would have given it.
// A row w writes is changed in w (so the import_progress_prior it records is the
// row it leaves); any other is stamped here.
func (c *Catalog) keepRewound(ctx context.Context, tx *sql.Tx, userID int64, rewound []progressPrior, w *ImportWrite) error {
	if len(rewound) == 0 {
		return nil
	}
	now := formatSessionTime(c.now())
	writes := make(map[Ref]*Progress, len(w.Progress))
	for i := range w.Progress {
		writes[w.Progress[i].Wrote.Ref] = &w.Progress[i].Wrote
	}
	for _, pr := range rewound {
		back := pr.restored()
		left, written := writes[pr.ref]
		if !written {
			left = &back
		}
		if IsNewer(*left, pr.wrote) {
			continue
		}
		if written {
			left.UpdatedAt = now
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE progress SET updated_at = ? WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
			now, userID, pr.ref.LibraryID, pr.ref.Path); err != nil {
			return err
		}
	}
	return nil
}
