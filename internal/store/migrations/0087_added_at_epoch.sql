-- server#360: books whose added_at a scan stamped before 1980, most often the
-- epoch itself, 1970-01-01. macOS over an rclone NFS share answers a birth time
-- of 0 for every file, which the scan took as when the book was added (Recently
-- added then sank them to its end, and "added in 1970" read as a bug). Scans
-- since take a birth time before 1980 as none and use the file's mtime
-- (library.birthKnown), but added_at is written only when a book is first
-- indexed, so the rows stamped before stay. Each takes the book's mtime (for a
-- book of several files its newest file's, where a scan now takes its earliest:
-- the index keeps no other), or, with no mtime from after 1980 either, when the
-- server last indexed it (to the second, as added_at is kept). An mtime past
-- what SQLite's date functions read (year 9999) is none: strftime would answer
-- NULL, which added_at (NOT NULL) refuses.
UPDATE books SET added_at = CASE
        WHEN mtime BETWEEN 315532800 AND 253402300799 THEN strftime('%Y-%m-%dT%H:%M:%SZ', mtime, 'unixepoch')
        WHEN indexed_at >= '1980' THEN substr(indexed_at, 1, 19) || 'Z'
        ELSE added_at
    END
  WHERE added_at <> '' AND added_at < '1980';
