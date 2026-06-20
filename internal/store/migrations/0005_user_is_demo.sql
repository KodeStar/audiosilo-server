-- Mark throwaway accounts created by public demo mode so the background reaper
-- deletes them by an explicit flag rather than by guessing from the "demo_"
-- username prefix. Reaping by prefix would silently delete any real account an
-- admin happened to name "demo_*"; the flag makes demo accounts unambiguous.
--
-- Existing rows default to 0 (not demo). Demo accounts are created with is_demo=1
-- by auth.CreateDemoUser.

ALTER TABLE users ADD COLUMN is_demo INTEGER NOT NULL DEFAULT 0;

-- Supports the reaper's "WHERE is_demo = 1" sweep and the capacity count.
CREATE INDEX idx_users_demo ON users(is_demo);
