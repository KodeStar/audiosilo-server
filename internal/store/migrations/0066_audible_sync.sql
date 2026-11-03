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
-- land behind the Bridge's cursor and never reach Audible. It is per person,
-- one more than their highest on every insert and every change of a row's
-- place, length, finish, version, device or time (the triggers below, so no
-- write path can miss it); the triggers' own UPDATE sets only change_seq,
-- which no UPDATE OF list names, so it never fires them again. Existing rows
-- are numbered in their updated_at order.
ALTER TABLE progress ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
UPDATE progress SET change_seq = (
    SELECT n FROM (
        SELECT rowid AS rid, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY updated_at, library_id, rel_path) AS n
          FROM progress) ranked
     WHERE ranked.rid = progress.rowid);
CREATE INDEX idx_progress_user_change ON progress(user_id, change_seq);

CREATE TRIGGER progress_change_seq_insert AFTER INSERT ON progress
BEGIN
    UPDATE progress SET change_seq = (SELECT COALESCE(MAX(change_seq), 0) + 1 FROM progress WHERE user_id = NEW.user_id)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER progress_change_seq_update
AFTER UPDATE OF position, duration, finished, version, device_id, updated_at ON progress
BEGIN
    UPDATE progress SET change_seq = (SELECT COALESCE(MAX(change_seq), 0) + 1 FROM progress WHERE user_id = NEW.user_id)
     WHERE rowid = NEW.rowid;
END;
