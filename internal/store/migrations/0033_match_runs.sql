-- Bulk community matching (Health > Not matched > Find matches): a run matches
-- many books against the community metadata at once and records what it found for
-- the admin to review; nothing changes until the admin applies it. A record of the
-- index, not durable user state: the newest few runs are kept (catalog.StartMatchRun
-- drops the rest).

-- One run. library_id NULL = every library. mode: match (books with no ASIN or
-- ISBN) | repick (books whose community ASIN may have one in the preferred
-- marketplace now). status: matching | ready | applying | applied | cancelled |
-- failed | interrupted. A run still matching at startup belonged to a server that
-- stopped mid-run (interrupted); one still applying goes back to ready, since the
-- items it applied are marked and the rest can be applied again.
CREATE TABLE match_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id  INTEGER REFERENCES libraries(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL,
    region      TEXT NOT NULL DEFAULT '',  -- metadata.region when it started
    status      TEXT NOT NULL,
    started_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,                      -- when matching ended
    total       INTEGER NOT NULL DEFAULT 0, -- books to match
    done        INTEGER NOT NULL DEFAULT 0, -- books matched so far
    scope       TEXT NOT NULL DEFAULT '',  -- the last apply's: ids | fill | overwrite
    apply_total INTEGER NOT NULL DEFAULT 0,
    apply_done  INTEGER NOT NULL DEFAULT 0,
    applied_at  TEXT,
    error       TEXT NOT NULL DEFAULT ''   -- why a run failed (a code)
);

-- One book a run matched, path-keyed. outcome: auto (confident: applied unless
-- the admin leaves it out) | review (a candidate, not confident) | none | error.
-- proposal is the community values the best candidate offers (JSON, see
-- catalog.MatchProposal). applied: '' | applied | skipped | failed.
CREATE TABLE match_run_items (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES match_runs(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    outcome    TEXT NOT NULL,
    score      INTEGER NOT NULL DEFAULT 0,
    runner_up  INTEGER NOT NULL DEFAULT 0, -- the next candidate's score
    proposal   TEXT NOT NULL DEFAULT '{}',
    applied    TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_match_run_items_run ON match_run_items(run_id, outcome, id);
-- The run's counts (catalog.matchRunCols) read the index alone.
CREATE INDEX idx_match_run_items_counts ON match_run_items(run_id, outcome, applied);
