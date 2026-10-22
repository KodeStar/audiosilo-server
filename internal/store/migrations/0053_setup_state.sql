-- First-run setup (FIRST-RUN-PLAN.md Phase F1): the admin console's "Set up your
-- server" flow keeps its progress in server_state under 'setup_state'
-- (catalog/setup.go). No row means a fresh server: the flow opens on the first
-- /admin visit.
--
-- A server upgraded from a release without the flow is already set up, so its
-- admins never get the flow or the Overview card: when it holds a library (of any
-- kind) or a second account (a demo visitor or a household member doesn't count),
-- it is marked finished and dismissed. A fresh database has neither when this
-- runs (the first admin is made after the migrations), so it gets no row.
INSERT OR IGNORE INTO server_state(key, value, updated_at)
SELECT 'setup_state', '{"finished":true,"dismissed":true,"steps":{}}', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
 WHERE EXISTS(SELECT 1 FROM libraries)
    OR (SELECT COUNT(*) FROM users WHERE is_demo = 0 AND managed_by IS NULL) > 1;
