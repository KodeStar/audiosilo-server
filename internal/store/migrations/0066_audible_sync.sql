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

-- audible_titles.ignored (0063, never set before 4c) is the console's
-- "Don't sync this book": neither direction touches the title's book. It
-- says nothing about backups.
