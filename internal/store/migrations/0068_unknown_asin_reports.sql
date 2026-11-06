-- LibraForge C3: the opt-in daily report of the Audible ASINs the community
-- database doesn't know (internal/coverage, metadata.report_unknown). One row per
-- ASIN the job has looked up: when it last asked (checked_at), whether that
-- answer was "unknown" (unknown 1; 0 = known, never sent), and when it was last
-- sent to the metadata service (sent_at, NULL for never; set only on a 202).
-- Names no book and no path: a row whose ASIN no book holds any more is deleted
-- at the next run. Derived and disposable (a lost table only means the ASINs are
-- looked up and sent again); written through the user-state path (no content
-- cache reads it). MAX(sent_at) keeps the report to once a day across restarts.
CREATE TABLE unknown_asin_reports (
    asin       TEXT PRIMARY KEY,
    checked_at TEXT NOT NULL,
    unknown    INTEGER NOT NULL DEFAULT 0,
    sent_at    TEXT
) WITHOUT ROWID;
