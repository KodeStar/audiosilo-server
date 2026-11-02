-- server#289 (Sync v2): a device's span chain lasts across sync batches. The
-- progress row remembers the clock-corrected end of the span its place came
-- from (span_at), recorded at the row's version then (span_version). While the
-- row is still at that version, the same device's next span compares with
-- span_at, not with updated_at (which a diverged apply keeps at a later time
-- of another device's, server#284), exactly as the next span of the same POST
-- does; so a backlog split across POSTs (413 halving, a long outbox) is not
-- answered "older". Every other writer clears them (catalog's endSpanChain),
-- which ends the chain: NULL or a stale version = today's rule.
ALTER TABLE progress ADD COLUMN span_at TEXT;
ALTER TABLE progress ADD COLUMN span_version INTEGER;
