ALTER TABLE podcast_feed_state RENAME TO podcast_feed_state_old;
CREATE TABLE podcast_feed_state (
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
 show_path TEXT NOT NULL, show_id TEXT NOT NULL,
 etag TEXT NOT NULL DEFAULT '', last_modified TEXT NOT NULL DEFAULT '', last_checked_at TEXT,
 last_success_at TEXT, last_status INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '', secret_missing INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0, next_poll_at TEXT,
 PRIMARY KEY(library_id,show_path,show_id)
);
INSERT INTO podcast_feed_state
 SELECT st.library_id,st.show_path,s.show_id,st.etag,st.last_modified,st.last_checked_at,
 st.last_success_at,st.last_status,st.error_code,st.error,st.secret_missing,st.failures,st.next_poll_at
 FROM podcast_feed_state_old st JOIN podcast_shows s ON s.library_id=st.library_id AND s.show_path=st.show_path;
DROP TABLE podcast_feed_state_old;
CREATE TABLE podcast_policies (
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
 show_path TEXT NOT NULL, show_id TEXT NOT NULL,
 mode TEXT NOT NULL CHECK(mode IN ('keep','on_demand','archive')),
 keep INTEGER NOT NULL CHECK(keep BETWEEN 1 AND 1000),
 days INTEGER NOT NULL CHECK(days BETWEEN 0 AND 365), updated_at TEXT NOT NULL,
 PRIMARY KEY(library_id,show_path,show_id)
);
CREATE TABLE podcast_fetch_state (
 library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
 rel_path TEXT NOT NULL, show_id TEXT NOT NULL, last_job_id INTEGER,
 failures INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT,
 error_code TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(library_id,rel_path,show_id)
);
ALTER TABLE podcast_episodes ADD COLUMN sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE podcast_episodes ADD COLUMN timeline_changed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE podcast_episodes ADD COLUMN fetch_json TEXT;
ALTER TABLE podcast_episodes ADD COLUMN feed_sig TEXT NOT NULL DEFAULT '';
