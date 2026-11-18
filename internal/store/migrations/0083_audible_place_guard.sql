-- Audible sync (server#303): a progress row names an Audible account as its
-- device ("audible:<account>") only while it holds a place that account's own
-- sync wrote. GET /bridge/progress leaves an account's own rows out (no echo),
-- so a place another writer put there under that device would never reach
-- Audible, and Audible's next place could undo it unseen. Writers used to
-- clear the device by hand (an edit, an import and its undo); these triggers
-- are the one rule, so no write path can miss it: any insert, and any update
-- that moves a row's place (its position or its finish), leaves the row with
-- no device and no device name when it carries an Audible one. The Audible
-- route (catalog.SyncListening, a furthest-only batch) names its device again
-- in a statement of its own right after writing its place (ownAudiblePlace),
-- which sets only device_id and device_name, so it fires neither trigger. A
-- carried (moved or joined) row the carry inserts, or whose place it moves,
-- loses the device too: the feed then offers Audible its own place once, which
-- the Bridge drops as the place it last heard (a merge that leaves the row's
-- place as it was keeps the device it names). The triggers' own update sets
-- neither position nor finished, so it never fires them again
-- (progress_change_seq_update moves the row's change_seq, which offers the
-- place to Audible: a guarded write draws two counter numbers, and a gap is
-- harmless, a number is never reused).
CREATE TRIGGER progress_audible_place_insert AFTER INSERT ON progress
WHEN NEW.device_id GLOB 'audible:*'
BEGIN
    UPDATE progress SET device_id = '', device_name = '' WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER progress_audible_place_update AFTER UPDATE OF position, finished ON progress
WHEN NEW.device_id GLOB 'audible:*'
 AND (NEW.position IS NOT OLD.position OR NEW.finished IS NOT OLD.finished)
BEGIN
    UPDATE progress SET device_id = '', device_name = '' WHERE rowid = NEW.rowid;
END;
