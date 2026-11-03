-- Bridge Phase 4a: Audible accounts. The Bridge signs in to an Audible account
-- (its credentials stay in the Bridge's vault, never here), maps it to a server
-- user and a books library, and reports the titles the account owns after each
-- poll. The server stores the mapping and the titles for the console and queues
-- the scheduled polls (audible.poll); it never talks to Audible.

-- One Audible account the Bridge signed in to. account is the Bridge's opaque
-- id (^[a-z0-9]{8,32}$). library_id NULL is a paused account: its library was
-- deleted, so no poll is queued until the Bridge maps it again. last_queued_at
-- is when the scheduler (or Check now) last queued its poll; last_poll_at when
-- the Bridge last finished one (its final titles call).
CREATE TABLE audible_accounts (
    account        TEXT PRIMARY KEY,
    label          TEXT NOT NULL DEFAULT '',
    marketplace    TEXT NOT NULL,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id     INTEGER REFERENCES libraries(id) ON DELETE SET NULL,
    policy         TEXT NOT NULL DEFAULT 'manual',
    sync           INTEGER NOT NULL DEFAULT 1,
    poll_hours     INTEGER NOT NULL DEFAULT 6,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    last_poll_at   TEXT,
    last_queued_at TEXT
) WITHOUT ROWID;
CREATE INDEX idx_audible_accounts_user ON audible_accounts(user_id);
CREATE INDEX idx_audible_accounts_library ON audible_accounts(library_id);

-- A title an account owns, as its last poll reported it. state is available,
-- preorder, unavailable or gone (absent from the account's last complete
-- poll). The match is the book on this server the Bridge matched it to:
-- path-keyed, no FK to the index (a rebuild keeps it), carried by a move
-- (catalog.moveBookState) and cleared with its library. rejected, ignored,
-- dest_path and import_job_id are the console's and the import's (Phase 4b);
-- a poll never changes them. poll_id is the poll that last reported the title.
CREATE TABLE audible_titles (
    account          TEXT NOT NULL REFERENCES audible_accounts(account) ON DELETE CASCADE,
    asin             TEXT NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    authors          TEXT NOT NULL DEFAULT '[]',
    series           TEXT NOT NULL DEFAULT '[]',
    release_date     TEXT NOT NULL DEFAULT '',
    runtime_s        INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL,
    reason           TEXT NOT NULL DEFAULT '',
    match_library_id INTEGER,
    match_path       TEXT,
    match_how        TEXT NOT NULL DEFAULT '',
    rejected         INTEGER NOT NULL DEFAULT 0,
    ignored          INTEGER NOT NULL DEFAULT 0,
    dest_path        TEXT NOT NULL DEFAULT '',
    first_seen       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    poll_id          TEXT NOT NULL DEFAULT '',
    import_job_id    INTEGER,
    PRIMARY KEY (account, asin)
) WITHOUT ROWID;
CREATE INDEX idx_audible_titles_match ON audible_titles(match_library_id, match_path);
CREATE INDEX idx_audible_titles_title ON audible_titles(title COLLATE NOCASE, account, asin);
