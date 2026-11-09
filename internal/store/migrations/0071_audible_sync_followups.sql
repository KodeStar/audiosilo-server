-- bridge#60: a title still a preorder takes no part in its account's sync
-- (catalog syncTitleSQL), as one gone from the account takes none: the poll
-- may have matched it to another edition on disk, and Audible holds no place
-- for an unreleased ASIN. Released (preorder -> available), it offers its
-- book's existing place to Audible again, as a title back from gone does.
DROP TRIGGER audible_title_match_update;
CREATE TRIGGER audible_title_match_update
AFTER UPDATE OF match_library_id, match_path, runtime_s, state, rejected, ignored, no_sync ON audible_titles
WHEN NEW.match_path IS NOT NULL
 AND NEW.rejected = 0 AND NEW.ignored = 0 AND NEW.no_sync = 0 AND NEW.state NOT IN ('gone', 'preorder')
 AND (OLD.match_path IS NOT NEW.match_path OR OLD.match_library_id IS NOT NEW.match_library_id
      OR OLD.runtime_s IS NOT NEW.runtime_s
      OR OLD.rejected <> 0 OR OLD.ignored <> 0 OR OLD.no_sync <> 0 OR OLD.state IN ('gone', 'preorder'))
BEGIN
    UPDATE progress_change_counter SET seq = seq + 1 WHERE id = 1;
    UPDATE progress SET change_seq = (SELECT seq FROM progress_change_counter WHERE id = 1)
     WHERE library_id = NEW.match_library_id AND rel_path = NEW.match_path
       AND user_id = (SELECT user_id FROM audible_accounts WHERE account = NEW.account);
END;

-- server#303: when the account's sync refusal last ended (a mapping, a fresh
-- sign-in, or a complete poll: Audible answered the account), so a refused
-- audible.sync claimed before it, which reaches the server after, does not
-- mark the account refused again (catalog.FoldAudibleSync). NULL: never.
ALTER TABLE audible_accounts ADD COLUMN refusal_ended_at TEXT;
