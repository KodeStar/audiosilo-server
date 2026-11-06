-- Bridge Phase 4c: Audible position sync both ways. The Bridge reads each
-- synced account's places on Audible (the audible.sync job, queued every 15
-- minutes) and posts them for the account's person
-- (POST /bridge/audible/accounts/{account}/listening, furthest only), and
-- pushes the person's places here back to Audible (GET /bridge/progress).

-- sync was stored from the Bridge's mapping since 0063 but never acted on.
-- From 4c the console owns it (PATCH /admin/audible/accounts/{account}) and it
-- is off until an admin turns it on, so every account starts off; a new
-- account is created off whatever the Bridge sends.
UPDATE audible_accounts SET sync = 0;

-- sync_queued_at is when the scheduler last queued the account's audible.sync,
-- last_sync_at when one last finished done. refused_at is when the account's
-- sync last failed with audible_refused (the Bridge's "Sign in again" state):
-- no sync is queued until the Bridge maps the account again (a fresh sign-in
-- PUTs the mapping), which clears it.
ALTER TABLE audible_accounts ADD COLUMN sync_queued_at TEXT;
ALTER TABLE audible_accounts ADD COLUMN last_sync_at TEXT;
ALTER TABLE audible_accounts ADD COLUMN refused_at TEXT;

-- no_sync is the console's "Don't sync this book" (PATCH
-- /admin/audible/titles/{account}/{asin} {no_sync}): neither direction touches
-- the title's book. It is the sync's alone (backups don't read it) and, like
-- rejected and ignored, a poll keeps it. A title also stays out of the sync
-- while unmatched, rejected or ignored.
ALTER TABLE audible_titles ADD COLUMN no_sync INTEGER NOT NULL DEFAULT 0;

-- change_seq orders a person's progress rows by when this server last wrote
-- them (GET /bridge/progress pages on it), not by their updated_at, which is the
-- listening's time: an offline backlog uploaded late, a diverged apply or late
-- finish keeping the later time, or a client's clock behind would otherwise
-- land behind the Bridge's cursor and never reach Audible. It comes from one
-- server-wide counter (progress_change_counter), bumped on every insert and
-- every change of a row's place, length, finish, version, device or time (the
-- triggers below, so no write path can miss it): strictly increasing and never
-- reused, so a deleted row can't hand its number to the next write (which a
-- cursor already past it would skip). The triggers' own UPDATE of progress sets
-- only change_seq, which no UPDATE OF list names, so it never fires them again.
-- Existing rows are numbered in their updated_at order.
ALTER TABLE progress ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
UPDATE progress SET change_seq = (
    SELECT n FROM (
        SELECT rowid AS rid, ROW_NUMBER() OVER (ORDER BY updated_at, user_id, library_id, rel_path) AS n
          FROM progress) ranked
     WHERE ranked.rid = progress.rowid);
CREATE INDEX idx_progress_user_change ON progress(user_id, change_seq);

CREATE TABLE progress_change_counter (
    id  INTEGER PRIMARY KEY CHECK (id = 1),
    seq INTEGER NOT NULL
);
INSERT INTO progress_change_counter(id, seq) SELECT 1, COALESCE(MAX(change_seq), 0) FROM progress;

CREATE TRIGGER progress_change_seq_insert AFTER INSERT ON progress
BEGIN
    UPDATE progress_change_counter SET seq = seq + 1 WHERE id = 1;
    UPDATE progress SET change_seq = (SELECT seq FROM progress_change_counter WHERE id = 1)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER progress_change_seq_update
AFTER UPDATE OF position, duration, finished, version, device_id, updated_at ON progress
BEGIN
    UPDATE progress_change_counter SET seq = seq + 1 WHERE id = 1;
    UPDATE progress SET change_seq = (SELECT seq FROM progress_change_counter WHERE id = 1)
     WHERE rowid = NEW.rowid;
END;

-- A title matched to a book (a new title, or one matched to another book), one
-- synced again (Don't sync this book cleared, un-rejected, un-ignored, back from
-- gone) or one whose runtime changed (a row the feed left out for a length that
-- disagreed may agree now) offers that book's existing place to Audible again:
-- its person's row there moves to the counter's next number, so a cursor
-- already past it reads it again (the Bridge drops a place it already holds).
CREATE TRIGGER audible_title_match_insert AFTER INSERT ON audible_titles
WHEN NEW.match_path IS NOT NULL
BEGIN
    UPDATE progress_change_counter SET seq = seq + 1 WHERE id = 1;
    UPDATE progress SET change_seq = (SELECT seq FROM progress_change_counter WHERE id = 1)
     WHERE library_id = NEW.match_library_id AND rel_path = NEW.match_path
       AND user_id = (SELECT user_id FROM audible_accounts WHERE account = NEW.account);
END;

CREATE TRIGGER audible_title_match_update
AFTER UPDATE OF match_library_id, match_path, runtime_s, state, rejected, ignored, no_sync ON audible_titles
WHEN NEW.match_path IS NOT NULL
 AND NEW.rejected = 0 AND NEW.ignored = 0 AND NEW.no_sync = 0 AND NEW.state <> 'gone'
 AND (OLD.match_path IS NOT NEW.match_path OR OLD.match_library_id IS NOT NEW.match_library_id
      OR OLD.runtime_s IS NOT NEW.runtime_s
      OR OLD.rejected <> 0 OR OLD.ignored <> 0 OR OLD.no_sync <> 0 OR OLD.state = 'gone')
BEGIN
    UPDATE progress_change_counter SET seq = seq + 1 WHERE id = 1;
    UPDATE progress SET change_seq = (SELECT seq FROM progress_change_counter WHERE id = 1)
     WHERE library_id = NEW.match_library_id AND rel_path = NEW.match_path
       AND user_id = (SELECT user_id FROM audible_accounts WHERE account = NEW.account);
END;
