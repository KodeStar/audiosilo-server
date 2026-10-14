-- Household members (player redesign Phase 8b-1, capability household_members):
-- any non-demo person adds a member from the player, with no admin. A member is
-- a real user whose managed_by is the person who added them (the owner), with a
-- member_kind (kid | grownup) and a member_access (all | collections) that only
-- the owner sets. All three are NULL for everyone else, or all set: SQLite can't
-- add a table CHECK with ALTER TABLE, so auth (members.go) keeps that rule.
--
-- auth.DeleteUser handles an owner's members itself, in its own transaction: it
-- clears all three member columns on the members the admin chose to keep
-- (keepMembers) and deletes the rest. managed_by's SET NULL is only a backstop
-- for a users row deleted some other way, which would leave the other two
-- columns set.
ALTER TABLE users ADD COLUMN managed_by INTEGER NULL REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN member_kind TEXT NULL CHECK (member_kind IN ('kid','grownup'));
ALTER TABLE users ADD COLUMN member_access TEXT NULL CHECK (member_access IN ('all','collections'));
-- The admin's per-person switch: may this person add members?
ALTER TABLE users ADD COLUMN can_add_members INTEGER NOT NULL DEFAULT 1;
-- The name the owner typed ("Evie"; '' for everyone else): the members list
-- shows it instead of the derived username.
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_users_managed_by ON users(managed_by) WHERE managed_by IS NOT NULL;

-- Which of the owner's devices (tokens) a member's sign-in was made on, so signing
-- that device out can sign the member out there too. Information only: a token's
-- validity never depends on it.
ALTER TABLE tokens ADD COLUMN created_on_token_id INTEGER NULL REFERENCES tokens(id) ON DELETE SET NULL;
CREATE INDEX idx_tokens_created_on ON tokens(created_on_token_id) WHERE created_on_token_id IS NOT NULL;
