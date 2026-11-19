-- A bookmark's client id (capability bookmark_client_id): the player names
-- each bookmark it saves with a UUID of its own, so a POST it replays from its
-- outbox after the first answer was lost finds the bookmark that POST made
-- instead of saving a second one (catalog.AddBookmark). NULL for a bookmark
-- saved without one (an older player, an Audiobookshelf import). One listener
-- holds a client id once; another listener's same id is theirs. A deleted
-- bookmark's id is free again, so an Undo that posts it once more saves anew.
ALTER TABLE bookmarks ADD COLUMN client_id TEXT;
CREATE UNIQUE INDEX idx_bookmarks_client_id ON bookmarks(user_id, client_id) WHERE client_id IS NOT NULL;
