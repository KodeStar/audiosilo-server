ALTER TABLE libraries ADD COLUMN kind TEXT NOT NULL DEFAULT 'books' CHECK (kind IN ('books','podcasts'));
CREATE TABLE podcast_shows (
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE, show_path TEXT NOT NULL,
 show_id TEXT NOT NULL, title TEXT NOT NULL, author TEXT NOT NULL, description TEXT NOT NULL,
 link TEXT NOT NULL, language TEXT NOT NULL, show_type TEXT NOT NULL, explicit INTEGER NOT NULL,
 private INTEGER NOT NULL, feed_url TEXT NOT NULL, feed_host TEXT NOT NULL, feed_url_sha256 TEXT NOT NULL,
 podcast_guid TEXT NOT NULL, cover_path TEXT NOT NULL, episode_count INTEGER NOT NULL,
 latest_published TEXT, feed_sig TEXT NOT NULL, indexed_at TEXT NOT NULL,
 PRIMARY KEY(library_id, show_path)
);
CREATE TABLE podcast_feed_hashes (
 library_id INTEGER NOT NULL, show_path TEXT NOT NULL, sha256 TEXT NOT NULL,
 PRIMARY KEY(library_id, sha256, show_path),
 FOREIGN KEY(library_id, show_path) REFERENCES podcast_shows(library_id, show_path) ON DELETE CASCADE
);
CREATE TABLE podcast_episodes (
 book_id INTEGER PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE, show_path TEXT NOT NULL,
 episode_id TEXT NOT NULL, guid TEXT NOT NULL, published TEXT, season INTEGER, episode INTEGER,
 episode_type TEXT NOT NULL, explicit INTEGER NOT NULL, feed_duration REAL NOT NULL,
 present INTEGER NOT NULL, generation INTEGER NOT NULL, removed_at TEXT NOT NULL,
 duration_source TEXT NOT NULL
);
CREATE INDEX idx_podcast_episodes_show ON podcast_episodes(library_id, show_path, published DESC);
CREATE TABLE podcast_follows (
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE, show_path TEXT NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY(user_id, library_id, show_path)
);
CREATE INDEX idx_podcast_follows_show ON podcast_follows(library_id, show_path);
CREATE TABLE podcast_feed_state (
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE, show_path TEXT NOT NULL,
 etag TEXT NOT NULL DEFAULT '', last_modified TEXT NOT NULL DEFAULT '', last_checked_at TEXT,
 last_success_at TEXT, last_status INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0, next_poll_at TEXT,
 PRIMARY KEY(library_id, show_path)
);
CREATE TABLE podcast_folded(job_id INTEGER PRIMARY KEY, at TEXT NOT NULL);

-- A request's display label is safe to retain after its volatile credential is gone.
ALTER TABLE bridge_jobs ADD COLUMN feed_redacted TEXT NOT NULL DEFAULT '';

-- A listener's follow requests (GET /me/podcasts, the open-request cap) and the
-- scheduler's sweep of ended podcast jobs to fold.
CREATE INDEX idx_bridge_jobs_subscribe_user ON bridge_jobs(requested_by, id) WHERE kind = 'podcast.subscribe';
CREATE INDEX idx_bridge_jobs_kind_finished ON bridge_jobs(kind, finished_at);
