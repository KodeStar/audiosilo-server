-- Per-library display order (lower sorts first). Drives the order libraries are
-- listed in, and is the tiebreaker when de-duplicating copies of the same book
-- that appear in more than one library (search / recently-added): with all else
-- equal, the copy in the earlier-ordered library wins. Existing libraries default
-- to 0 and fall back to name order until an admin reorders them.
ALTER TABLE libraries ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
