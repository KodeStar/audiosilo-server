-- Bridge Phase 4b: Audible backups (the audible.import job). The Bridge
-- downloads a title the account owns, decrypts it for the account and places it
-- in the account's library; the server decides what to back up and when.

-- template is the account's path template for a backup's place in the library
-- ('' = the Bridge's automatic placement beside the series' other books),
-- set in the console. policy_since is when the account's automatic policy
-- (all or series) began to act: only a title first seen after it is backed up
-- by the policy (new purchases only; the existing library is the console's
-- one-off "Back up everything"). It is set when an account is created, when
-- the console turns an automatic policy on, and moved to the account's first
-- complete poll (catalog.PutAudibleTitles), so that poll's titles count as the
-- library the account already had. An account that already polled starts from
-- its last poll, one that never did from its creation.
ALTER TABLE audible_accounts ADD COLUMN template TEXT NOT NULL DEFAULT '';
ALTER TABLE audible_accounts ADD COLUMN policy_since TEXT NOT NULL DEFAULT '';
UPDATE audible_accounts SET policy_since = COALESCE(last_poll_at, created_at);

-- import_pending is a console "Back up" on a title that can't be backed up yet
-- (a preorder): the import is queued once a poll reports it available.
-- A title's import otherwise is import_job_id, the bridge_jobs row.
ALTER TABLE audible_titles ADD COLUMN import_pending INTEGER NOT NULL DEFAULT 0;
