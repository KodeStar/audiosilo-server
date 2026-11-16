-- Sync v2, "Stay wins" (row 9c-2): a listener who answers another device's
-- offer with Stay sends a listen span with start "stay", and the server applies
-- its place even when the other device kept writing after the offer appeared
-- (catalog's resolveListen). stay_version is the version a Stay wrote; while it
-- is set, another device's span on a base below it never moves the place (it is
-- offered instead). The Stay's own device writing again keeps it; any other
-- writer clears it (another device's applied span, a PUT, a PATCH, an import, a
-- move or join, a finish or unfinish). edit_version is the version of the row's
-- last explicit write that is not listening (a PATCH, an import's apply or undo,
-- a move or join, a sync finish or unfinish; never a PUT, which is an old app's
-- listening): a Stay made on a base below it is a plain jump. NULL = none.
ALTER TABLE progress ADD COLUMN stay_version INTEGER;
ALTER TABLE progress ADD COLUMN edit_version INTEGER;
