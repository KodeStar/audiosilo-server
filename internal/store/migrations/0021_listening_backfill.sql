-- Listening from before the server recorded sessions (migration 0018), so the
-- Activity pages don't start at the upgrade. Two sources, best first:
--
-- 1. The players' listening spans (listening_history: one row per stretch of
--    playback, with its real start and end times; every player since June 2026
--    posts them). Spans that ended before a person's first recorded session become
--    sessions, joined like live ones (a gap of more than 10 minutes, SessionGap,
--    starts a new one), marked backfilled: the device, app and playback mode were
--    never recorded, so they count in time, books and people but not in devices or
--    playback modes. A span's listening is its wall-clock length, at most twice
--    the position it moved (0.5x, the slowest speed a player offers), so a player
--    left "playing" without moving adds nothing.
--
-- 2. What the spans don't cover (listening before players sent them, downloaded
--    books played offline): for each book a person first saved before 0018
--    (progress.started_at is only stamped on a row's first save), the position
--    reached at their speed, less every session recorded for it (not the whole
--    book for one marked finished: it may have been marked, not played; a player
--    that plays to the end saves the end position). Kept as one estimated day per
--    book (5 minutes or more),
--    dated by the earliest thing known about it (its first session, or else its
--    last save, in UTC: the day may be off by one near midnight). Estimates count
--    in a period's totals and its top books, authors, narrators and listeners, but
--    never in a day-by-day chart, a calendar or the hours of the day.
--
-- Session ids page the session lists newest first, so the existing sessions move
-- up to leave the low ids to the backfilled ones, in the order they happened. No
-- table refers to a session id.

ALTER TABLE listening_sessions ADD COLUMN backfilled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE listening_daily ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0;

CREATE TEMP TABLE backfill_sessions AS
WITH spans AS (
    SELECT h.user_id, h.library_id, h.rel_path, h.from_pos, h.to_pos,
           strftime('%Y-%m-%dT%H:%M:%fZ', h.started_at) AS s,
           strftime('%Y-%m-%dT%H:%M:%fZ', h.ended_at) AS e
      FROM listening_history h
),
firsts AS (
    SELECT user_id, MIN(started_at) AS first FROM listening_sessions GROUP BY user_id
),
counted AS (
    SELECT sp.*,
           MIN(ROUND(unixepoch(sp.e, 'subsec') - unixepoch(sp.s, 'subsec'), 3),
               ABS(sp.to_pos - sp.from_pos) * 2) AS listened
      FROM spans sp LEFT JOIN firsts f ON f.user_id = sp.user_id
     WHERE sp.s IS NOT NULL AND sp.e IS NOT NULL AND sp.e > sp.s
       AND sp.e < COALESCE(f.first, '9999')
),
marked AS (
    SELECT *, CASE WHEN unixepoch(s, 'subsec') - unixepoch(LAG(e) OVER w, 'subsec') <= 600 THEN 0 ELSE 1 END AS starts
      FROM counted
    WINDOW w AS (PARTITION BY user_id, library_id, rel_path ORDER BY s, e)
),
grouped AS (
    SELECT *, SUM(starts) OVER (PARTITION BY user_id, library_id, rel_path ORDER BY s, e
                                ROWS UNBOUNDED PRECEDING) AS sitting
      FROM marked
),
ends AS (
    SELECT *,
           FIRST_VALUE(from_pos) OVER g AS start_pos,
           LAST_VALUE(to_pos) OVER g AS end_pos
      FROM grouped
    WINDOW g AS (PARTITION BY user_id, library_id, rel_path, sitting ORDER BY s, e
                 ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
)
SELECT user_id, library_id, rel_path, MIN(s) AS started_at, MAX(e) AS last_at,
       MIN(start_pos) AS start_pos, MIN(end_pos) AS end_pos, SUM(listened) AS listened
  FROM ends
 GROUP BY user_id, library_id, rel_path, sitting
HAVING SUM(listened) > 0;

-- One pass: past the highest id plus the backfill, nothing can collide (the gap
-- left below is harmless).
UPDATE listening_sessions
   SET id = id + (SELECT MAX(id) FROM listening_sessions) + (SELECT COUNT(*) FROM temp.backfill_sessions);

INSERT INTO listening_sessions(id, user_id, library_id, rel_path, started_at, last_at,
                               start_pos, end_pos, duration, speed, listened, backfilled)
SELECT ROW_NUMBER() OVER (ORDER BY b.started_at, b.user_id, b.library_id, b.rel_path),
       b.user_id, b.library_id, b.rel_path, b.started_at, b.last_at, b.start_pos, b.end_pos,
       COALESCE(p.duration, 0),
       CASE WHEN p.playback_speed BETWEEN 0.25 AND 4 THEN p.playback_speed ELSE 1 END,
       b.listened, 1
  FROM temp.backfill_sessions b
  LEFT JOIN progress p ON p.user_id = b.user_id AND p.library_id = b.library_id AND p.rel_path = b.rel_path;

UPDATE sqlite_sequence SET seq = (SELECT COALESCE(MAX(id), 0) FROM listening_sessions)
 WHERE name = 'listening_sessions';

DROP TABLE temp.backfill_sessions;

WITH recorded AS (
    SELECT user_id, library_id, rel_path, SUM(listened) AS listened, MIN(started_at) AS first
      FROM listening_sessions GROUP BY user_id, library_id, rel_path
),
rolled AS (
    SELECT user_id, library_id, rel_path, SUM(listened) AS listened
      FROM listening_daily GROUP BY user_id, library_id, rel_path
),
books AS (
    SELECT p.user_id, p.library_id, p.rel_path,
           p.position / CASE WHEN p.playback_speed BETWEEN 0.25 AND 4 THEN p.playback_speed ELSE 1 END
             - COALESCE(r.listened, 0) - COALESCE(d.listened, 0) AS estimate,
           date(COALESCE(r.first, p.updated_at)) AS day
      FROM progress p
      JOIN users u ON u.id = p.user_id AND u.is_demo = 0
      LEFT JOIN recorded r ON r.user_id = p.user_id AND r.library_id = p.library_id AND r.rel_path = p.rel_path
      LEFT JOIN rolled d ON d.user_id = p.user_id AND d.library_id = p.library_id AND d.rel_path = p.rel_path
     WHERE p.started_at IS NULL
)
INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions, estimated)
SELECT day, user_id, library_id, rel_path, estimate, 0, 1
  FROM books
 WHERE estimate >= 300 AND day IS NOT NULL;
