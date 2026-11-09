-- server#332: the scopes under which a refresh item had nothing to change when
-- the run recorded it (comma-separated, e.g. "fill"): what the review showed as
-- no change. An apply under one of them leaves the item out without reading the
-- book again (catalog.MatchItemsToApply), so it never writes a change the admin
-- never saw, and an empty refresh's Done costs nothing per item. No backfill: ''
-- (an item recorded before 0071, a match or repick item) is planned as before.
ALTER TABLE match_run_items ADD COLUMN unchanged TEXT NOT NULL DEFAULT '';
