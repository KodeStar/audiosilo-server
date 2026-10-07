package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Bulk community matching (internal/matchrun runs it): a match_runs row per run
// and a match_run_items row per book it matched, holding what the best community
// candidate offers so the admin can review it before anything changes. Records of
// the index, not durable user state, kept to the newest maxMatchRuns.

// maxMatchRuns bounds how many runs (with their items) are kept.
const maxMatchRuns = 10

// ErrRunNotReady marks an apply asked of a run that isn't waiting for one (still
// matching, already applying or applied, stopped).
var ErrRunNotReady = errors.New("the match run is not ready to apply")

// Match run modes.
const (
	// MatchModeMatch matches the books with no ASIN or ISBN.
	MatchModeMatch = "match"
	// MatchModeRepick looks again at books whose ASIN a community match set, for
	// the same recording's ASIN in the preferred marketplace.
	MatchModeRepick = "repick"
)

// Match run statuses.
const (
	MatchMatching    = "matching"
	MatchReady       = "ready" // matched; waiting for the admin to apply
	MatchApplying    = "applying"
	MatchApplied     = "applied"
	MatchCancelled   = "cancelled"
	MatchFailed      = "failed"
	MatchInterrupted = "interrupted"
)

// Match item outcomes.
const (
	OutcomeAuto   = "auto"   // confident: applied unless the admin leaves it out
	OutcomeReview = "review" // a candidate, not confident enough to apply unasked
	OutcomeNone   = "none"   // no candidate
	OutcomeError  = "error"  // the community service failed for this book
)

// What applying did to an item.
const (
	ItemApplied = "applied"
	ItemSkipped = "skipped" // nothing to change, or the book is gone
	ItemFailed  = "failed"
)

// MatchRunCounts are a run's items by outcome, and what applying did. Pending is
// the confident items not applied yet (what an apply takes unless told otherwise).
type MatchRunCounts struct {
	Auto    int `json:"auto"`
	Pending int `json:"pending"`
	Review  int `json:"review"`
	None    int `json:"none"`
	Error   int `json:"error"`
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// MatchRun is one bulk match run.
type MatchRun struct {
	ID            int64          `json:"id"`
	LibraryID     *int64         `json:"library_id"` // nil = every library
	LibraryName   string         `json:"library_name,omitempty"`
	Mode          string         `json:"mode"`
	Region        string         `json:"region"`
	Status        string         `json:"status"`
	StartedBy     *int64         `json:"started_by"`
	StartedByName string         `json:"started_by_name,omitempty"`
	StartedAt     string         `json:"started_at"`
	FinishedAt    *string        `json:"finished_at"`
	Total         int            `json:"total"`
	Done          int            `json:"done"`
	Scope         string         `json:"scope"`
	ApplyTotal    int            `json:"apply_total"`
	ApplyDone     int            `json:"apply_done"`
	AppliedAt     *string        `json:"applied_at"`
	Error         string         `json:"error,omitempty"`
	Counts        MatchRunCounts `json:"counts"`
}

// MatchProposal is what the best community candidate offers a book: the work and
// recording it is, how to show it, and the value it has for each overridable
// field (in stored form; a field it has nothing for is absent).
type MatchProposal struct {
	WorkID      string            `json:"work_id,omitempty"`
	RecordingID string            `json:"recording_id,omitempty"`
	Title       string            `json:"title,omitempty"`
	Authors     string            `json:"authors,omitempty"`
	Narrators   string            `json:"narrators,omitempty"`
	RuntimeMin  int               `json:"runtime_min,omitempty"`
	WebURL      string            `json:"web_url,omitempty"`
	CoverURL    string            `json:"cover_url,omitempty"`
	ASINRegion  string            `json:"asin_region,omitempty"` // the proposed ASIN's marketplace
	Values      map[string]string `json:"values"`
}

// MatchRunItem is one book a run matched.
type MatchRunItem struct {
	ID        int64         `json:"id"`
	RunID     int64         `json:"run_id"`
	LibraryID int64         `json:"library_id"`
	Path      string        `json:"path"`
	Outcome   string        `json:"outcome"`
	Score     int           `json:"score"`
	RunnerUp  int           `json:"runner_up"`
	Proposal  MatchProposal `json:"proposal"`
	Applied   string        `json:"applied"`
	Detail    string        `json:"detail,omitempty"`
}

// StartMatchRun records a run that starts matching run.Total books and returns
// its id, dropping the oldest runs past maxMatchRuns (never an active one).
func (c *Catalog) StartMatchRun(ctx context.Context, run MatchRun) (int64, error) {
	var id int64
	err := c.db.WithTx(ctx, "StartMatchRun", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO match_runs(library_id, mode, region, status, started_by, started_at, total)
			 VALUES(?,?,?,?,?,?,?)`,
			run.LibraryID, run.Mode, run.Region, MatchMatching, run.StartedBy, c.ts(), run.Total)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM match_runs WHERE status NOT IN (?, ?) AND id NOT IN
			   (SELECT id FROM match_runs ORDER BY id DESC LIMIT ?)`,
			MatchMatching, MatchApplying, maxMatchRuns)
		return err
	})
	return id, err
}

// RecordMatchItem stores one matched book's item (nil = nothing worth keeping, as
// a repick that found no better ASIN) and counts the book done. The item of a book
// whose library was deleted since the run listed it is left out (the library's
// items go with it), never failing the run; ErrNotFound when the run itself is
// gone (its library was deleted).
func (c *Catalog) RecordMatchItem(ctx context.Context, runID int64, item *MatchRunItem) error {
	return c.db.WithTx(ctx, "RecordMatchItem", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE match_runs SET done = done + 1 WHERE id = ?`, runID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if item == nil {
			return nil
		}
		raw, err := json.Marshal(item.Proposal)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO match_run_items(run_id, library_id, path, outcome, score, runner_up, proposal, detail)
			 SELECT ?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM libraries WHERE id = ?)`,
			runID, item.LibraryID, item.Path, item.Outcome, item.Score, item.RunnerUp, string(raw), item.Detail,
			item.LibraryID)
		return err
	})
}

// FinishMatching ends a run's matching: ready, cancelled or failed (with a code).
func (c *Catalog) FinishMatching(ctx context.Context, runID int64, status, errCode string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE match_runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errCode, c.ts(), runID)
	return err
}

// BeginApply moves a ready run to applying total items with scope.
// ErrRunNotReady when the run isn't ready (another apply got there first, or it
// never finished matching).
func (c *Catalog) BeginApply(ctx context.Context, runID int64, scope string, total int) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE match_runs SET status = ?, scope = ?, apply_total = ?, apply_done = 0
		  WHERE id = ? AND status = ?`, MatchApplying, scope, total, runID, MatchReady)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRunNotReady
	}
	return nil
}

// MarkMatchItem records what applying did to an item and counts it done.
func (c *Catalog) MarkMatchItem(ctx context.Context, runID, itemID int64, applied, detail string) error {
	return c.db.WithTx(ctx, "MarkMatchItem", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE match_run_items SET applied = ?, detail = ? WHERE id = ? AND run_id = ?`,
			applied, detail, itemID, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE match_runs SET apply_done = apply_done + 1 WHERE id = ?`, runID)
		return err
	})
}

// FinishApply ends an apply: applied, or (cancelled) back to ready, its applied
// items marked, so the rest can be applied later.
func (c *Catalog) FinishApply(ctx context.Context, runID int64, cancelled bool) error {
	if cancelled {
		_, err := c.db.ExecContext(ctx, `UPDATE match_runs SET status = ? WHERE id = ?`, MatchReady, runID)
		return err
	}
	_, err := c.db.ExecContext(ctx,
		`UPDATE match_runs SET status = ?, applied_at = ? WHERE id = ?`, MatchApplied, c.ts(), runID)
	return err
}

// InterruptMatchRuns settles runs a previous server left working: one matching
// is interrupted; one applying goes back to ready (what it applied is marked).
// Called once at startup, before any run can start.
func (c *Catalog) InterruptMatchRuns(ctx context.Context) error {
	return c.db.WithTx(ctx, "InterruptMatchRuns", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE match_runs SET status = ?, finished_at = COALESCE(finished_at, started_at) WHERE status = ?`,
			MatchInterrupted, MatchMatching); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE match_runs SET status = ? WHERE status = ?`, MatchReady, MatchApplying)
		return err
	})
}

// matchRunCols reads a run with its counts, from one pass over its items that
// idx_match_run_items_counts covers (the console polls the runs each second while
// one works).
const matchRunCols = `r.id, r.library_id, COALESCE(l.name, ''), r.mode, r.region, r.status, r.started_by,
	COALESCE(u.username, ''), r.started_at, r.finished_at, r.total, r.done, r.scope, r.apply_total,
	r.apply_done, r.applied_at, r.error, COALESCE(n.auto, 0), COALESCE(n.pending, 0),
	COALESCE(n.review, 0), COALESCE(n.none, 0), COALESCE(n.error, 0), COALESCE(n.applied, 0),
	COALESCE(n.skipped, 0), COALESCE(n.failed, 0)
	FROM match_runs r LEFT JOIN libraries l ON l.id = r.library_id LEFT JOIN users u ON u.id = r.started_by
	LEFT JOIN (SELECT run_id,
	                  SUM(outcome = '` + OutcomeAuto + `') AS auto,
	                  SUM(outcome = '` + OutcomeAuto + `' AND applied = '') AS pending,
	                  SUM(outcome = '` + OutcomeReview + `') AS review,
	                  SUM(outcome = '` + OutcomeNone + `') AS none,
	                  SUM(outcome = '` + OutcomeError + `') AS error,
	                  SUM(applied = '` + ItemApplied + `') AS applied,
	                  SUM(applied = '` + ItemSkipped + `') AS skipped,
	                  SUM(applied = '` + ItemFailed + `') AS failed
	             FROM match_run_items GROUP BY run_id) n ON n.run_id = r.id`

func scanMatchRun(row interface{ Scan(...any) error }, r *MatchRun) error {
	n := &r.Counts
	return row.Scan(&r.ID, &r.LibraryID, &r.LibraryName, &r.Mode, &r.Region, &r.Status, &r.StartedBy,
		&r.StartedByName, &r.StartedAt, &r.FinishedAt, &r.Total, &r.Done, &r.Scope, &r.ApplyTotal,
		&r.ApplyDone, &r.AppliedAt, &r.Error,
		&n.Auto, &n.Pending, &n.Review, &n.None, &n.Error, &n.Applied, &n.Skipped, &n.Failed)
}

// ListMatchRuns returns the kept runs, newest first.
func (c *Catalog) ListMatchRuns(ctx context.Context) ([]MatchRun, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, r *MatchRun) error {
		return scanMatchRun(rows, r)
	}, `SELECT `+matchRunCols+` ORDER BY r.id DESC LIMIT ?`, maxMatchRuns)
}

// GetMatchRun returns one run (ErrNotFound when there is none).
func (c *Catalog) GetMatchRun(ctx context.Context, id int64) (*MatchRun, error) {
	var r MatchRun
	err := scanMatchRun(c.db.QueryRowContext(ctx, `SELECT `+matchRunCols+` WHERE r.id = ?`, id), &r)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

func scanMatchItem(rows *sql.Rows, it *MatchRunItem) error {
	var raw string
	if err := rows.Scan(&it.ID, &it.RunID, &it.LibraryID, &it.Path, &it.Outcome, &it.Score, &it.RunnerUp,
		&raw, &it.Applied, &it.Detail); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), &it.Proposal); err != nil {
		return err
	}
	if it.Proposal.Values == nil {
		it.Proposal.Values = map[string]string{}
	}
	return nil
}

const matchItemCols = `id, run_id, library_id, path, outcome, score, runner_up, proposal, applied, detail`

// ListMatchRunItems returns a page of a run's items in id order: of one outcome
// ("" = all), after the item `after`, at most limit, and the id the next page
// reads after (0 when this is the last).
func (c *Catalog) ListMatchRunItems(ctx context.Context, runID int64, outcome string, after int64, limit int) ([]MatchRunItem, int64, error) {
	items, err := queryRows(ctx, c.db, scanMatchItem, `SELECT `+matchItemCols+` FROM match_run_items
	     WHERE run_id = ? AND (? = '' OR outcome = ?) AND id > ? ORDER BY id LIMIT ?`,
		runID, outcome, outcome, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	items, next := pageBefore(items, limit, func(it MatchRunItem) int64 { return it.ID })
	return items, next, nil
}

// MatchItemsToApply lists the items an apply works through: the run's confident
// ones except those in exclude, plus those in include that have a candidate (a
// "review" item the admin chose), each only while not yet applied.
func (c *Catalog) MatchItemsToApply(ctx context.Context, runID int64, include, exclude []int64) ([]MatchRunItem, error) {
	all, err := queryRows(ctx, c.db, scanMatchItem, `SELECT `+matchItemCols+` FROM match_run_items
	     WHERE run_id = ? AND applied = '' AND outcome IN (?, ?) ORDER BY id`, runID, OutcomeAuto, OutcomeReview)
	if err != nil {
		return nil, err
	}
	set := func(ids []int64) map[int64]bool {
		m := make(map[int64]bool, len(ids))
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	in, out := set(include), set(exclude)
	return slices.DeleteFunc(all, func(it MatchRunItem) bool {
		if it.Outcome == OutcomeAuto {
			return out[it.ID]
		}
		return !in[it.ID]
	}), nil
}

// matchSubjectCols are the facts a match reads of a book.
const matchSubjectCols = `b.id, b.library_id, b.rel_path, b.is_folder, b.title, b.author, b.series,
	b.series_index, b.narrator, b.duration, b.asin, b.isbn`

func scanMatchSubject(rows *sql.Rows, b *Book) error {
	return rows.Scan(&b.ID, &b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author, &b.Series,
		&b.SeriesIndex, &b.Narrator, &b.Duration, &b.ASIN, &b.ISBN)
}

// UnmatchedBooks lists the books a match run works through: those with no ASIN
// or ISBN (the Health page's "Not matched", the same predicate) in one library
// (0 = every library), less the ones an admin ignored there, with the facts a
// match reads.
func (c *Catalog) UnmatchedBooks(ctx context.Context, libraryID int64) ([]Book, error) {
	return queryRows(ctx, c.db, scanMatchSubject, `SELECT `+matchSubjectCols+` FROM books b
	     WHERE (?1 = 0 OR b.library_id = ?1) AND `+issuePredicates[IssueUnmatched]+`
	       AND NOT `+ignoredExpr+`
	     ORDER BY b.library_id, b.rel_path`, libraryID, IssueUnmatched)
}

// CommunityASINBooks lists the books whose ASIN an admin accepted from a community
// match (a book_overrides row of source community): what a repick run reconsiders.
// An ASIN from the tags, an admin's own edit or the manager's enrichment is never
// one.
func (c *Catalog) CommunityASINBooks(ctx context.Context, libraryID int64) ([]Book, error) {
	return queryRows(ctx, c.db, scanMatchSubject, `SELECT `+matchSubjectCols+` FROM books b
	     WHERE (?1 = 0 OR b.library_id = ?1) AND b.asin <> '' AND EXISTS(SELECT 1 FROM book_overrides o
	           WHERE o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?2 AND o.source = ?3)
	     ORDER BY b.library_id, b.rel_path`, libraryID, FieldASIN, SourceCommunity)
}

// MatchState is what applying a match to a book needs to know of it: each field's
// effective value and source (an admin's own edit is never replaced), and whether
// it has no cover at all (a scan checked and found none, and none was uploaded).
type MatchState struct {
	Title        string
	Author       string
	Fields       map[string]FieldValue
	CoverMissing bool
}

// BookMatchState loads a book's MatchState (ErrNotFound when none is indexed at
// the path).
func (c *Catalog) BookMatchState(ctx context.Context, libraryID int64, relPath string) (*MatchState, error) {
	var (
		id int64
		st MatchState
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT b.id, b.title, b.author, `+issuePredicates[IssueNoCover]+` FROM books b
		  WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, CleanRelPath(relPath)).
		Scan(&id, &st.Title, &st.Author, &st.CoverMissing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l, err := loadLayers(ctx, c.db, id)
	if err != nil {
		return nil, err
	}
	st.Fields = l.resolve()
	return &st, nil
}

// CleanOverride is value as an override of field would store it, "" when the
// field would refuse it (a community value that doesn't fit is left out, never
// failing a whole edit).
func CleanOverride(field, value string) string {
	v, err := normalizeOverride(field, value)
	if err != nil {
		return ""
	}
	return v
}
