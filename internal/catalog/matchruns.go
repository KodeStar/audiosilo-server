package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// ErrMatchRunBusy marks an Unlink asked while a run that covers the book's
// library (or every library) is applying (clearing the book then would let the
// apply write its match or cover back), and is matchrun.ErrBusy, a run started
// or applied while another works.
var ErrMatchRunBusy = errors.New("a match run is already working")

// Match run modes.
const (
	// MatchModeMatch matches the books not matched yet: no ASIN or ISBN and no
	// recorded community match (UnmatchedBooks).
	MatchModeMatch = "match"
	// MatchModeRepick looks again at books whose ASIN a community match set, for
	// the same recording's ASIN in the preferred marketplace.
	MatchModeRepick = "repick"
	// MatchModeRefresh looks the books with an ASIN or ISBN up by it, and a book
	// matched with neither by its recorded community match (MatchedBooks), to fill
	// in the details the match didn't bring (an enrichment, an apply of ids only).
	MatchModeRefresh = "refresh"
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
	OutcomeNone   = "none"   // no candidate, or only weak ones
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
	Error         string         `json:"error,omitempty"` // matchrun's ErrCode* (see there)
	Counts        MatchRunCounts `json:"counts"`
}

// MatchProposal is what the best community candidate offers a book: the work and
// recording it is, how to show it, and the value it has for each overridable
// field (in stored form; a field it has nothing for is absent).
type MatchProposal struct {
	WorkID      string `json:"work_id,omitempty"`
	RecordingID string `json:"recording_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Authors     string `json:"authors,omitempty"`
	Narrators   string `json:"narrators,omitempty"`
	RuntimeMin  int    `json:"runtime_min,omitempty"`
	WebURL      string `json:"web_url,omitempty"`
	CoverURL    string `json:"cover_url,omitempty"`
	ASINRegion  string `json:"asin_region,omitempty"` // the proposed ASIN's marketplace
	// FoundBy is the book's own identifier a refresh found the record by, or
	// the ASIN a repick looked up, "asin:<ASIN>" or "isbn:<ISBN>", or for a
	// refresh by its recorded match, "work:<work id>"
	// then "/<recording id>" when it names one ("" for a match run, and a repick
	// recorded before 2.4.0). A fallback after identifier misses appends
	// ";asin:<ASIN>;isbn:<ISBN>" (including empty values): applying it requires
	// the same recorded match and both identifiers unchanged.
	FoundBy string            `json:"found_by,omitempty"`
	Values  map[string]string `json:"values"`
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
	// Unchanged is the scopes a refresh item had nothing to change under when
	// the run recorded it (migration 0070): what the review showed as no change,
	// so an apply under one leaves the item out (MatchItemsToApply). Internal and
	// write-only: RecordMatchItem stores it, the apply filters on it in SQL, and
	// an item read back leaves it empty.
	Unchanged []string `json:"-"`
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
			`INSERT INTO match_run_items(run_id, library_id, path, outcome, score, runner_up, proposal, detail, unchanged)
			 SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM libraries WHERE id = ?)`,
			runID, item.LibraryID, item.Path, item.Outcome, item.Score, item.RunnerUp, string(raw), item.Detail,
			strings.Join(item.Unchanged, ","), item.LibraryID)
		return err
	})
}

// FinishMatching ends a run's matching: ready, cancelled or failed (with a code;
// a ready run an outage stopped early carries one too).
func (c *Catalog) FinishMatching(ctx context.Context, runID int64, status, errCode string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE match_runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errCode, c.ts(), runID)
	return err
}

// BeginApply moves a ready run to applying total items with scope.
// ErrRunNotReady when the run isn't ready (another apply got there first, or it
// never finished matching). A transaction, not a lone statement: an autocommit
// write under modernc can answer a context that ended after it committed with
// the context's error, which would leave the run applying with no apply to
// finish it (and every Unlink refused, ErrMatchRunBusy); a transaction reports
// a failure only for a write it rolled back.
func (c *Catalog) BeginApply(ctx context.Context, runID int64, scope string, total int) error {
	return c.db.WithTx(ctx, "BeginApply", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
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
	})
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

// DropApplyItems leaves n items out of a working apply: its total counts n
// fewer (never below what it has done), and the items are not marked (still
// pending, as an exclusion leaves them). A refresh's items that had a change
// under the scope when recorded but have none now (server#332), counted up and
// dropped in batches, so the apply ends at its total without the console
// excluding each such item.
func (c *Catalog) DropApplyItems(ctx context.Context, runID int64, n int) error {
	if n <= 0 {
		return nil
	}
	return c.db.WithTx(ctx, "DropApplyItems", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE match_runs SET apply_total = max(apply_total - ?, apply_done) WHERE id = ?`, n, runID)
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
// Called once at startup, before any run can start (SettleMatchRuns of every
// run).
func (c *Catalog) InterruptMatchRuns(ctx context.Context) error {
	_, err := c.SettleMatchRuns(ctx, 0)
	return err
}

// SettleMatchRuns settles every run but keep (0 = none kept) still marked
// working that no worker is behind: matching -> interrupted, applying -> ready
// (what it applied is marked, the rest still to apply). One transaction; it
// returns how many runs it settled. Such a run is one a stopped server left, or
// one whose finishing write failed (FinishMatching, FinishApply), which would
// otherwise refuse every Unlink of its library until a restart. Only the caller
// knows no worker is behind a run (matchrun.Runner, under its lock with no run
// active, or keeping the active one).
func (c *Catalog) SettleMatchRuns(ctx context.Context, keep int64) (int, error) {
	return c.settleMatchRuns(ctx, "SettleMatchRuns", keep, false)
}

// SettleMatchRun settles run id as SettleMatchRuns does, when it is still
// marked working: false when it isn't (finished, or no such run).
func (c *Catalog) SettleMatchRun(ctx context.Context, id int64) (bool, error) {
	n, err := c.settleMatchRuns(ctx, "SettleMatchRun", id, true)
	return n > 0, err
}

// settleMatchRuns settles the working run id when only, else every working
// run but id.
// An interrupted run's finished_at is when it started, unless it has one: when
// it stopped is unknown.
func (c *Catalog) settleMatchRuns(ctx context.Context, op string, id int64, only bool) (int, error) {
	var settled int64
	err := c.db.WithTx(ctx, op, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE match_runs
			    SET status = CASE status WHEN ?1 THEN ?2 ELSE ?3 END,
			        finished_at = CASE status WHEN ?1 THEN COALESCE(finished_at, started_at) ELSE finished_at END
			  WHERE status IN (?1, ?4) AND (id = ?5) = ?6`,
			MatchMatching, MatchInterrupted, MatchReady, MatchApplying, id, only)
		if err != nil {
			return err
		}
		settled, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return int(settled), nil
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

// ListMatchRunItems returns a page of a run's items, the best match first (score
// descending, then id): of one outcome ("" = all), after the item `after`, at most
// limit, and the id the next page reads after (0 when this is the last). The
// cursor stays an item id: the page reads on from that item's score, which never
// changes once recorded, so a review paged 50 at a time sees the whole outcome in
// one order rather than each page sorted on its own. An `after` that isn't one of
// the run's items reads nothing (the row it compares with is all NULL).
func (c *Catalog) ListMatchRunItems(ctx context.Context, runID int64, outcome string, after int64, limit int) ([]MatchRunItem, int64, error) {
	items, err := queryRows(ctx, c.db, scanMatchItem, `SELECT `+matchItemCols+` FROM match_run_items
	     WHERE run_id = ? AND (? = '' OR outcome = ?)
	       AND (? = 0 OR (score, -id) < (SELECT score, -id FROM match_run_items WHERE id = ? AND run_id = ?))
	     ORDER BY score DESC, id LIMIT ?`,
		runID, outcome, outcome, after, after, runID, limit+1)
	if err != nil {
		return nil, 0, err
	}
	items, next := pageBefore(items, limit, func(it MatchRunItem) int64 { return it.ID })
	return items, next, nil
}

// MatchItemsToApply lists the items an apply under scope works through: the
// run's confident ones except those in exclude, plus those in include that have a
// candidate (a "review" item the admin chose), each only while not yet applied,
// and never one recorded with nothing to change under scope (MatchRunItem.
// Unchanged: a refresh item the review showed no change for, left out unread,
// as an exclusion leaves it, whatever the book would take now: server#332).
func (c *Catalog) MatchItemsToApply(ctx context.Context, runID int64, scope string, include, exclude []int64) ([]MatchRunItem, error) {
	all, err := queryRows(ctx, c.db, scanMatchItem, `SELECT `+matchItemCols+` FROM match_run_items
	     WHERE run_id = ? AND applied = '' AND outcome IN (?, ?)
	       AND instr(',' || unchanged || ',', ',' || ? || ',') = 0 ORDER BY id`,
		runID, OutcomeAuto, OutcomeReview, scope)
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
// The book's recorded community match (book_matches) rides along by a LEFT
// JOIN on its primary key, so every query naming these columns joins
// matchSubjectJoin after `books b`.
const matchSubjectCols = `b.id, b.library_id, b.rel_path, b.is_folder, b.title, b.author, b.series,
	b.series_index, b.series_index_end, b.narrator, b.duration, b.asin, b.isbn, b.language,
	b.tag_asin, b.tag_isbn, b.tag_language, b.genres, COALESCE(bm.work_id, ''), COALESCE(bm.recording_id, '')`

// matchSubjectJoin is the join matchSubjectCols reads the match from.
const matchSubjectJoin = ` LEFT JOIN book_matches bm ON bm.library_id = b.library_id AND bm.path = b.rel_path`

func scanMatchSubject(rows *sql.Rows, b *Book) error {
	var genres string
	if err := rows.Scan(&b.ID, &b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author, &b.Series,
		&b.SeriesIndex, &b.SeriesIndexEnd, &b.Narrator, &b.Duration, &b.ASIN, &b.ISBN, &b.Language,
		&b.TagASIN, &b.TagISBN, &b.TagLanguage, &genres, &b.MatchWorkID, &b.MatchRecordingID); err != nil {
		return err
	}
	b.Genres = ParseLabels(genres)
	return nil
}

// LanguageCount is how many books of a library say one language: their language
// (Book.Language) and their tags' (Book.TagLanguage), as stored.
type LanguageCount struct {
	Language, TagLanguage string
	Books                 int
}

// LanguageCounts counts the books of a library by the language each says, every
// book of it, matched or not: what a match derives the library's language from
// (matchrun.LibraryLanguage). One grouped read, so a run reads it once per library;
// the values come raw, read the way a match reads a book's (meta.NormalizeLanguage).
func (c *Catalog) LanguageCounts(ctx context.Context, libraryID int64) ([]LanguageCount, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, n *LanguageCount) error {
		return rows.Scan(&n.Language, &n.TagLanguage, &n.Books)
	}, `SELECT b.language, b.tag_language, COUNT(*) FROM books b
	     WHERE b.library_id = ? AND `+inBookLibrary+`
	     GROUP BY b.language, b.tag_language`, libraryID)
}

// UnmatchedBooks lists the books a match run works through: those with no ASIN
// or ISBN and no recorded community match (the Health page's "Not matched", the
// same predicate) in one library
// (0 = every library), less the ones an admin ignored there, with the facts a
// match reads.
func (c *Catalog) UnmatchedBooks(ctx context.Context, libraryID int64) ([]Book, error) {
	return queryRows(ctx, c.db, scanMatchSubject, `SELECT `+matchSubjectCols+` FROM books b`+matchSubjectJoin+`
	     WHERE `+inBookLibrary+` AND (?1 = 0 OR b.library_id = ?1) AND `+issuePredicates[IssueUnmatched]+`
	       AND NOT `+ignoredExpr+`
	     ORDER BY b.library_id, b.rel_path`, libraryID, IssueUnmatched)
}

// MatchedBooks lists the matched books in one library (0 = every library): an
// ASIN or ISBN, or a recorded community match (book_matches) without one, which a
// refresh looks up by its work. What a refresh run looks up.
func (c *Catalog) MatchedBooks(ctx context.Context, libraryID int64) ([]Book, error) {
	return queryRows(ctx, c.db, scanMatchSubject, `SELECT `+matchSubjectCols+` FROM books b`+matchSubjectJoin+`
	     WHERE `+inBookLibrary+` AND (?1 = 0 OR b.library_id = ?1) AND `+matchedExpr+`
	     ORDER BY b.library_id, b.rel_path`, libraryID)
}

// CommunityASINBooks lists the books whose ASIN an admin accepted from a community
// match (a book_overrides row of source community): what a repick run reconsiders.
// An ASIN from an Audiobookshelf metadata.json, an admin's own edit or an
// enrichment (the Bridge's Audible backfill) is never one.
func (c *Catalog) CommunityASINBooks(ctx context.Context, libraryID int64) ([]Book, error) {
	return queryRows(ctx, c.db, scanMatchSubject, `SELECT `+matchSubjectCols+` FROM books b`+matchSubjectJoin+`
	     WHERE `+inBookLibrary+` AND (?1 = 0 OR b.library_id = ?1) AND b.asin <> '' AND EXISTS(SELECT 1 FROM book_overrides o
	           WHERE o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?2 AND o.source = ?3)
	     ORDER BY b.library_id, b.rel_path`, libraryID, FieldASIN, SourceCommunity)
}

// MatchState is what applying a match to a book needs to know of it: each field's
// effective value and source (an admin's own edit is never replaced), and whether
// it has no cover at all (a scan checked and found none, and none was uploaded).
type MatchState struct {
	Title  string
	Author string
	// Duration is the book's length in seconds (0 unknown).
	Duration     float64
	Fields       map[string]FieldValue
	CoverMissing bool
	// Anchor is the book's recorded community match (book_matches), zero when
	// it has none. How is MatchHowAuto or MatchHowAccepted; the offered
	// identifiers are those it offered when recorded (0070).
	Anchor MatchAnchor
}

// BookMatchState loads a book's MatchState (ErrNotFound when none is indexed at
// the path).
func (c *Catalog) BookMatchState(ctx context.Context, libraryID int64, relPath string) (*MatchState, error) {
	var (
		id int64
		st MatchState
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT b.id, b.title, b.author, b.duration, `+issuePredicates[IssueNoCover]+`,
		        COALESCE(bm.work_id, ''), COALESCE(bm.recording_id, ''), COALESCE(bm.how, ''),
		        COALESCE(bm.offered_asin, ''), COALESCE(bm.offered_isbn, '')
		   FROM books b`+matchSubjectJoin+`
		  WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, CleanRelPath(relPath)).
		Scan(&id, &st.Title, &st.Author, &st.Duration, &st.CoverMissing,
			&st.Anchor.WorkID, &st.Anchor.RecordingID, &st.Anchor.How,
			&st.Anchor.OfferedASIN, &st.Anchor.OfferedISBN)
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

// EditedValue is field's value as the book holds it when that is an admin's
// edit (SourceEdited, never a scanned, path, ABS or community value) saying the
// same as value (sameFieldValue), with ok false otherwise: the Bridge's
// meta.correct job files only an admin's own edits.
func (st *MatchState) EditedValue(field, value string) (edited string, ok bool) {
	fv := st.Fields[field]
	if fv.Source != SourceEdited || !sameFieldValue(field, fv.Value, value) {
		return "", false
	}
	return fv.Value, true
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

// ClearedMatches is what ClearCommunityMatches removed.
type ClearedMatches struct {
	Books  int `json:"books"`  // books that had a community value, cover or recorded match
	Covers int `json:"covers"` // community covers
	Runs   int `json:"runs"`   // match runs, with their reviews
}

// ClearCommunityMatches undoes the community matches of one library (0 = every
// library), so its books can be matched from fresh: its community overrides,
// covers and recorded matches (book_matches) go (an admin's own edits and
// uploads, the tags and an enrichment stay), each book's effective metadata and cover art are rebuilt, and
// the match runs go with their reviews (a run over every library loses only this
// library's books). One transaction. The caller makes sure no match run is
// working meanwhile (matchrun.Runner.Clear).
func (c *Catalog) ClearCommunityMatches(ctx context.Context, libraryID int64) (*ClearedMatches, error) {
	var out ClearedMatches
	err := c.db.WithTx(ctx, "ClearCommunityMatches", func(tx *sql.Tx) error {
		var err error
		if out.Books, out.Covers, err = clearCommunity(ctx, tx, libraryBooks(libraryID)); err != nil {
			return err
		}

		// The runs: every one (their items cascade), or this library's and its
		// books' items in the runs over every library.
		res, err := tx.ExecContext(ctx, `DELETE FROM match_runs WHERE ?1 = 0 OR library_id = ?1`, libraryID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		out.Runs = int(n)
		if libraryID == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM match_run_items WHERE library_id = ?
		     AND run_id IN (SELECT id FROM match_runs WHERE library_id IS NULL)`, libraryID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if out.Books > 0 {
		c.changed() // the books' identifiers may now be their tags' or enrichment's
	}
	return &out, nil
}

// clearPick is the books a clearCommunity takes: libraryBooks or oneBook, the
// only two, so its SQL is written here and never by a caller.
type clearPick struct {
	where string // over library_id and path
	args  []any
}

// libraryBooks picks every book of a library (0 = every library).
func libraryBooks(libraryID int64) clearPick {
	return clearPick{`(?1 = 0 OR library_id = ?1)`, []any{libraryID}}
}

// oneBook picks the one book at ref (its path clean).
func oneBook(ref Ref) clearPick {
	return clearPick{`library_id = ?1 AND path = ?2`, []any{ref.LibraryID, ref.Path}}
}

// clearCommunity removes the community overrides, covers and recorded matches
// (book_matches) of the books pick names, keeping an admin's own edits and
// uploads, and rebuilds each such indexed book's effective metadata and cover
// art, in tx. It returns how many books had any and how many community covers
// went: ClearCommunityMatches for a library, an unlink (BookEdit.Match with no
// work) for one book.
func clearCommunity(ctx context.Context, tx *sql.Tx, pick clearPick) (books, covers int, err error) {
	where, args := pick.where, pick.args
	scanRef := func(r *sql.Rows, ref *Ref) error { return r.Scan(&ref.LibraryID, &ref.Path) }
	fields, err := queryRows(ctx, tx, scanRef, `DELETE FROM book_overrides
	     WHERE source = 'community' AND `+where+` RETURNING library_id, path`, args...)
	if err != nil {
		return 0, 0, err
	}
	// 'community' written out (SourceCommunity), as the covers' partial index,
	// which lists them without reading an image, can't match a bound value.
	coverRefs, err := queryRows(ctx, tx, scanRef, `DELETE FROM book_covers
	     WHERE source = 'community' AND `+where+` RETURNING library_id, path`, args...)
	if err != nil {
		return 0, 0, err
	}
	anchors, err := queryRows(ctx, tx, scanRef, `DELETE FROM book_matches
	     WHERE `+where+` RETURNING library_id, path`, args...)
	if err != nil {
		return 0, 0, err
	}
	touched := map[Ref]bool{}
	for _, ref := range fields {
		if touched[ref] {
			continue // one row per field
		}
		touched[ref] = true
		id, err := bookIDByPath(ctx, tx, ref.LibraryID, ref.Path)
		if errors.Is(err, ErrNotFound) {
			continue // not indexed now: nothing to rebuild
		}
		if err != nil {
			return 0, 0, err
		}
		if err := refreshEffective(ctx, tx, id); err != nil {
			return 0, 0, err
		}
	}
	for _, ref := range coverRefs {
		touched[ref] = true
		if err := refreshCoverArt(ctx, tx, ref.LibraryID, ref.Path); err != nil {
			return 0, 0, err
		}
	}
	for _, ref := range anchors {
		touched[ref] = true // the row is all there is: nothing to rebuild
	}
	return len(touched), len(coverRefs), nil
}
