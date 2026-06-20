-- Drop the per-library `layout` column. Library shape is now auto-detected per
-- folder by the scanner, with optional per-folder overrides (folder_overrides),
-- so a single library-wide layout no longer has any meaning. The column is index
-- config, not user data; dropping it is safe (SQLite >= 3.35 supports it).
ALTER TABLE libraries DROP COLUMN layout;
