-- Which browser a sign-in came from, so "new device" is announced once per
-- browser rather than at every sign-in (the admin console signs in again after
-- each sign-out). The console keeps a random id in the browser's storage and sends
-- it with the password sign-in; the server keeps only its SHA-256 here (hex), on
-- the session it issued. A sign-in whose key a previous session of the same person
-- already carries is not a new device. Empty: the client sent none (a player, a
-- paired device, an API key), which is always announced. An admin signing a
-- session out from People > Devices blanks its key on every session of that
-- person, so the same browser is announced again if it comes back.
ALTER TABLE tokens ADD COLUMN sign_in_key TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_tokens_sign_in_key ON tokens(user_id, sign_in_key) WHERE sign_in_key <> '';
