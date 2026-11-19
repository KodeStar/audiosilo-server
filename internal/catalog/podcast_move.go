package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// A show folder renamed by hand keeps its feed.json, so its show identity
// (show_id) turns up at the new folder while the follows, feed state, policy
// and listening state stay keyed to the old one (path is the identity). The
// scanner finds such an identity (PodcastIdentityPaths), looks at the disk to
// tell a rename from a copy, and moves the state (MovePodcastShows).

// PodcastIdentityPath is one path holding a show identity's index row or its
// durable state (a follow, feed state, a retention policy).
type PodcastIdentityPath struct {
	Path string
	// Indexed: the path's index row (podcast_shows) is this identity's.
	Indexed bool
}

// PodcastIdentityPaths are lib's show identities found at more than one path,
// each with its paths in path order. Only an identity indexed at one path at
// least counts: one indexed nowhere has no folder to move to. With at not "",
// only the identities with a row or state at that path (a targeted rescan's
// show) are read.
func (c *Catalog) PodcastIdentityPaths(ctx context.Context, lib int64, at string) (map[string][]PodcastIdentityPath, error) {
	type row struct {
		id string
		p  PodcastIdentityPath
	}
	rows, err := queryRows(ctx, c.db, func(r *sql.Rows, v *row) error { return r.Scan(&v.id, &v.p.Path, &v.p.Indexed) },
		`WITH ids(show_path,show_id) AS (
		   SELECT show_path,show_id FROM podcast_shows WHERE library_id=?1
		   UNION SELECT show_path,show_id FROM podcast_follows WHERE library_id=?1
		   UNION SELECT show_path,show_id FROM podcast_feed_state WHERE library_id=?1
		   UNION SELECT show_path,show_id FROM podcast_policies WHERE library_id=?1)
		 SELECT i.show_id,i.show_path,EXISTS(SELECT 1 FROM podcast_shows s WHERE s.library_id=?1 AND s.show_path=i.show_path AND s.show_id=i.show_id)
		   FROM ids i
		  WHERE i.show_id IN (SELECT show_id FROM ids GROUP BY show_id HAVING COUNT(*)>1)
		    AND i.show_id IN (SELECT show_id FROM podcast_shows WHERE library_id=?1)
		    AND (?2='' OR i.show_id IN (SELECT show_id FROM ids WHERE show_path=?2))
		  ORDER BY i.show_id,i.show_path`, lib, at)
	if err != nil {
		return nil, err
	}
	out := map[string][]PodcastIdentityPath{}
	for _, r := range rows {
		out[r.id] = append(out[r.id], r.p)
	}
	return out, nil
}

// PodcastPathKnown reports whether lib's index or durable podcast state names
// folder show: an index row, a follow, feed state or a retention policy there.
// The scanner asks it of a root entry it can't look into for good (a
// root-owned lost+found): one known nowhere holds up neither a prune nor a move.
func (c *Catalog) PodcastPathKnown(ctx context.Context, lib int64, show string) (bool, error) {
	var known bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM podcast_shows WHERE library_id=?1 AND show_path=?2)
		 OR EXISTS(SELECT 1 FROM podcast_follows WHERE library_id=?1 AND show_path=?2)
		 OR EXISTS(SELECT 1 FROM podcast_feed_state WHERE library_id=?1 AND show_path=?2)
		 OR EXISTS(SELECT 1 FROM podcast_policies WHERE library_id=?1 AND show_path=?2)`, lib, show).Scan(&known)
	return known, err
}

// PodcastMove carries a show identity's state from the folders that no
// longer hold it (From) to the one folder that does (To).
type PodcastMove struct {
	ShowID string
	From   []string
	To     string
	// Files are the episode file names To's feed.json lists, whatever folder
	// its paths name (one renamed by hand names its old folder until the
	// Bridge takes it in). When that old name now holds another show (two
	// names swapped, or one reused) neither index row lists them any more.
	Files []string
}

// MovePodcastShows applies moves in one transaction and answers how many it
// applied. A move applies only while To's index row is ShowID's (else it is
// passed over: the folder changed since the caller looked), and is held while
// To carries the state of another identity that may still be on its way
// somewhere (unheldPodcastMoves): a follow, feed state or policy there of an
// identity indexed elsewhere (its own move skipped: a copy, a path that can't
// be told; any of the three keeps To's show-folder heart that identity's,
// podcastFolderStateOf) and, with unread (the caller could not read every
// folder the root lists, one of which may hold that identity now, or read
// none as no move asked for it when it looked, PodcastMovesOverOthers: that is
// decided here, in the transaction), any of the three there of another
// identity. Either is passed over when that identity is
// lifted from To in the same call (names swapped, a chain of renames). A
// listener's place in To holds nothing: To's index row is ShowID's by now, so
// a show whose only state there is keyed by path alone has no identity left
// there for a later move of its own, and the move carries only its own files'
// state. Of a From path it moves only ShowID's rows:
//
//   - every listener's follow (one at To already for the same identity keeps
//     the earlier date; a follow at To of another identity that is indexed
//     nowhere, or moves off To in this call, is replaced);
//   - the feed state (the later check wins a clash) and the retention policy
//     (the newer edit wins), both keyed by identity;
//   - the episodes' fetch backoffs (podcast_fetch_state);
//   - the listening state on the show folder itself (a show-folder heart),
//     which is keyed by path alone, so only while From's folder is not another
//     show's own: From's index row is ShowID's or there is none, or the show
//     it names has no follow, feed state or policy there (it was renamed into
//     the folder by hand and its own move is still to come: names swapped, a
//     chain of renames). The state on a folder an unrelated show holds under
//     that name in its own right stays that show's;
//   - the listening state on each episode, From/<file> to To/<file>
//     (carryListeningState), for the files listed at To, at From's own index
//     row and in Files. A place the listener already has at To/<file> (the
//     new folder listed the episode while the move waited) merges as two saves
//     that never saw each other: the further place is kept, a finished one
//     over any (showMoveCarry, mergeUnseen), never the newer save alone;
//   - From's index row and episodes, dropped when the row is ShowID's.
//
// Follows and the show folder's listening state go through a placeholder path
// first, so two shows whose folder names were swapped by hand trade places in
// one call. Bridge jobs stay where they were queued: the Bridge finds a renamed
// folder by identity, and an outcome naming the new folder folds there
// (FoldPodcastJob).
func (c *Catalog) MovePodcastShows(ctx context.Context, lib int64, moves []PodcastMove, unread bool) (int, error) {
	applied := 0
	err := c.db.WithTx(ctx, "MovePodcastShows", func(tx *sql.Tx) error {
		var live []PodcastMove
		for _, m := range moves {
			id, err := podcastShowIDAt(ctx, tx, lib, m.To)
			if err != nil {
				return err
			}
			if id != m.ShowID || !feed.ShowPath(m.To) {
				continue
			}
			m.From = slices.DeleteFunc(slices.Clone(m.From), func(from string) bool { return from == m.To || !feed.ShowPath(from) })
			if len(m.From) == 0 {
				continue
			}
			live = append(live, m)
		}
		live, err := unheldPodcastMoves(ctx, tx, lib, live, unread)
		if err != nil {
			return err
		}
		// Whether each From folder's own listening state goes with its move,
		// told before any row moves (a lift takes the follows this reads).
		folders := make([][]bool, len(live))
		for i, m := range live {
			for _, from := range m.From {
				folder, err := podcastFolderStateOf(ctx, tx, lib, from, m.ShowID)
				if err != nil {
					return err
				}
				folders[i] = append(folders[i], folder)
			}
		}
		for i, m := range live {
			for j, from := range m.From {
				if err := liftShowRows(ctx, tx, lib, m.ShowID, from, movePlaceholder(i), folders[i][j]); err != nil {
					return err
				}
			}
		}
		for i, m := range live {
			for _, from := range m.From {
				if err := moveShowState(ctx, tx, lib, m.ShowID, from, m.To, m.Files); err != nil {
					return err
				}
			}
			if err := placeShowRows(ctx, tx, lib, movePlaceholder(i), m.To); err != nil {
				return err
			}
		}
		applied = len(live)
		return nil
	})
	return applied, err
}

// unheldPodcastMoves are the moves podcastMoveHeld holds none of, looked at
// until no more is held: a held move lifts nothing from its From paths, so a
// move to one of those may be held in turn. Each To is read once
// (podcastOthersAt): only what is lifted changes from one look to the next.
func unheldPodcastMoves(ctx context.Context, tx *sql.Tx, lib int64, moves []PodcastMove, unread bool) ([]PodcastMove, error) {
	others := make([][]podcastOther, len(moves))
	for i, m := range moves {
		var err error
		if others[i], err = podcastOthersAt(ctx, tx, lib, m); err != nil {
			return nil, err
		}
	}
	for {
		lifted := map[[2]string]bool{} // {show_id, path}
		for _, m := range moves {
			for _, from := range m.From {
				lifted[[2]string{m.ShowID, from}] = true
			}
		}
		var keptMoves []PodcastMove
		var keptOthers [][]podcastOther
		for i, m := range moves {
			if !podcastMoveHeld(m.To, others[i], lifted, unread) {
				keptMoves, keptOthers = append(keptMoves, m), append(keptOthers, others[i])
			}
		}
		if len(keptMoves) == len(moves) {
			return moves, nil
		}
		moves, others = keptMoves, keptOthers
	}
}

// podcastOther is an identity with a follow, feed state or policy at a move's
// To; indexed, the identity is indexed somewhere in the library.
type podcastOther struct {
	id      string
	indexed bool
}

// PodcastMovesOverOthers reports whether one of moves' new folders (To)
// carries a follow, feed state or policy of another identity indexed nowhere:
// only such a move can wait on the caller's unread (podcastMoveHeld: one
// indexed elsewhere holds the move whatever unread says), so the scanner looks
// at the folders that may hold that identity only then. It reads outside
// MovePodcastShows' transaction, so its answer only saves the look: a caller
// that did not look passes unread, and the transaction decides.
func (c *Catalog) PodcastMovesOverOthers(ctx context.Context, lib int64, moves []PodcastMove) (bool, error) {
	for _, m := range moves {
		others, err := podcastOthersAt(ctx, c.db, lib, m)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(others, func(o podcastOther) bool { return !o.indexed }) {
			return true, nil
		}
	}
	return false, nil
}

// podcastOthersAt are the identities other than m's with state at m.To.
func podcastOthersAt(ctx context.Context, q rowQuerier, lib int64, m PodcastMove) ([]podcastOther, error) {
	return queryRows(ctx, q, func(r *sql.Rows, o *podcastOther) error { return r.Scan(&o.id, &o.indexed) },
		`WITH ids(show_id) AS (
		   SELECT show_id FROM podcast_follows WHERE library_id=?1 AND show_path=?2 AND show_id<>?3
		   UNION SELECT show_id FROM podcast_feed_state WHERE library_id=?1 AND show_path=?2 AND show_id<>?3
		   UNION SELECT show_id FROM podcast_policies WHERE library_id=?1 AND show_path=?2 AND show_id<>?3)
		 SELECT show_id,EXISTS(SELECT 1 FROM podcast_shows s WHERE s.library_id=?1 AND s.show_id=ids.show_id)
		   FROM ids`, lib, m.To, m.ShowID)
}

// podcastMoveHeld reports whether a move to to waits (MovePodcastShows says
// when): one of others, not lifted from to in this call (by lifted,
// {show_id, path}), is indexed elsewhere or, with unread, may be.
func podcastMoveHeld(to string, others []podcastOther, lifted map[[2]string]bool, unread bool) bool {
	for _, o := range others {
		if !lifted[[2]string{o.id, to}] && (unread || o.indexed) {
			return true
		}
	}
	return false
}

// podcastShowIDAt is the show identity of show's index row, "" with none.
func podcastShowIDAt(ctx context.Context, tx *sql.Tx, lib int64, show string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, show).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// podcastFolderStateOf reports whether the listening state on folder show
// itself (keyed by path alone) can be taken for show id's: show's index row is
// id's or there is none, or the show it names has no durable state there yet
// (a follow, feed state or a policy), so it only arrived by a rename by hand.
func podcastFolderStateOf(ctx context.Context, tx *sql.Tx, lib int64, show, id string) (bool, error) {
	held, err := podcastShowIDAt(ctx, tx, lib, show)
	if err != nil || held == "" || held == id {
		return err == nil, err
	}
	var settled bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM podcast_follows WHERE library_id=?1 AND show_path=?2 AND show_id=?3)
		 OR EXISTS(SELECT 1 FROM podcast_feed_state WHERE library_id=?1 AND show_path=?2 AND show_id=?3)
		 OR EXISTS(SELECT 1 FROM podcast_policies WHERE library_id=?1 AND show_path=?2 AND show_id=?3)`, lib, show, held).Scan(&settled)
	return !settled, err
}

// movePlaceholder is where MovePodcastShows holds move i's follows and show
// folder state between its two passes: a hidden name, which no show folder can
// have (feed.ShowPath), inside the transaction only.
func movePlaceholder(i int) string { return fmt.Sprintf(".podcast-move-%d", i) }

// liftShowRows moves id's follows at from to hold, and with folder the
// listening state on the folder itself (MovePodcastShows says when).
func liftShowRows(ctx context.Context, tx *sql.Tx, lib int64, id, from, hold string, folder bool) error {
	// A listener following at two of a move's From paths keeps one follow,
	// with the earlier date.
	for _, q := range []string{
		`INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at)
		 SELECT user_id,library_id,?3,show_id,created_at FROM podcast_follows WHERE library_id=?1 AND show_path=?2 AND show_id=?4
		 ON CONFLICT(user_id,library_id,show_path) DO UPDATE SET created_at=excluded.created_at
		  WHERE excluded.created_at<podcast_follows.created_at`,
		`DELETE FROM podcast_follows WHERE library_id=?1 AND show_path=?2 AND show_id=?4`,
	} {
		if _, err := tx.ExecContext(ctx, q, lib, from, hold, id); err != nil {
			return err
		}
	}
	if !folder {
		return nil
	}
	return carryListeningState(ctx, tx, lib, JoinPart{Path: from, Last: true}, hold, 0, showMoveCarry)
}

// placeShowRows moves what liftShowRows held to the show's folder.
func placeShowRows(ctx context.Context, tx *sql.Tx, lib int64, hold, to string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at)
		 SELECT user_id,library_id,?3,show_id,created_at FROM podcast_follows WHERE library_id=?1 AND show_path=?2
		 ON CONFLICT(user_id,library_id,show_path) DO UPDATE SET show_id=excluded.show_id,created_at=excluded.created_at
		  WHERE podcast_follows.show_id<>excluded.show_id OR excluded.created_at<podcast_follows.created_at`, lib, hold, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM podcast_follows WHERE library_id=? AND show_path=?`, lib, hold); err != nil {
		return err
	}
	return carryListeningState(ctx, tx, lib, JoinPart{Path: hold, Last: true}, to, 0, showMoveCarry)
}

// moveShowState moves id's feed state, policy, fetch backoffs and episodes'
// listening state from from to to, and drops from's index row when it is id's.
// listed are episode file names to's feed.json lists (PodcastMove.Files).
func moveShowState(ctx context.Context, tx *sql.Tx, lib int64, id, from, to string, listed []string) error {
	for _, q := range []string{
		`INSERT INTO podcast_feed_state(library_id,show_path,show_id,etag,last_modified,last_checked_at,last_success_at,last_status,error_code,error,secret_missing,failures,next_poll_at,feed_failures,feed_network_failures,failing_announced,failing_code)
		 SELECT library_id,?3,show_id,etag,last_modified,last_checked_at,last_success_at,last_status,error_code,error,secret_missing,failures,next_poll_at,feed_failures,feed_network_failures,failing_announced,failing_code
		   FROM podcast_feed_state WHERE library_id=?1 AND show_path=?2 AND show_id=?4
		 ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET etag=excluded.etag,last_modified=excluded.last_modified,
		     last_checked_at=excluded.last_checked_at,last_success_at=excluded.last_success_at,last_status=excluded.last_status,
		     error_code=excluded.error_code,error=excluded.error,secret_missing=excluded.secret_missing,failures=excluded.failures,
		     next_poll_at=excluded.next_poll_at,feed_failures=excluded.feed_failures,feed_network_failures=excluded.feed_network_failures,
		     failing_announced=excluded.failing_announced,failing_code=excluded.failing_code
		  WHERE COALESCE(excluded.last_checked_at,'')>COALESCE(podcast_feed_state.last_checked_at,'')`,
		`DELETE FROM podcast_feed_state WHERE library_id=?1 AND show_path=?2 AND show_id=?4`,
		`INSERT INTO podcast_policies(library_id,show_path,show_id,mode,keep,days,updated_at)
		 SELECT library_id,?3,show_id,mode,keep,days,updated_at FROM podcast_policies WHERE library_id=?1 AND show_path=?2 AND show_id=?4
		 ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET mode=excluded.mode,keep=excluded.keep,days=excluded.days,updated_at=excluded.updated_at
		  WHERE excluded.updated_at>podcast_policies.updated_at`,
		`DELETE FROM podcast_policies WHERE library_id=?1 AND show_path=?2 AND show_id=?4`,
		// The episodes under from/: '0' sorts right after '/'.
		`UPDATE OR IGNORE podcast_fetch_state SET rel_path=?3||substr(rel_path,length(?2)+1)
		  WHERE library_id=?1 AND rel_path>=?2||'/' AND rel_path<?2||'0' AND show_id=?4`,
		`DELETE FROM podcast_fetch_state WHERE library_id=?1 AND rel_path>=?2||'/' AND rel_path<?2||'0' AND show_id=?4`,
	} {
		if _, err := tx.ExecContext(ctx, q, lib, from, to, id); err != nil {
			return err
		}
	}
	// The episode files: those listed at to, those of from's own row while it
	// is still id's (to's index may not list them yet: a feed.json renamed by
	// hand names the old folder until the Bridge takes it in), and those to's
	// feed.json lists under any folder name (listed: from's row may already be
	// another show's).
	held, err := podcastShowIDAt(ctx, tx, lib, from)
	if err != nil {
		return err
	}
	stale := held == id
	files, err := queryRows(ctx, tx, func(r *sql.Rows, p *string) error { return r.Scan(p) },
		`SELECT b.rel_path FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE e.library_id=?1 AND e.show_path IN (?2,CASE WHEN ?3 THEN ?4 END)`, lib, to, stale, from)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, f := range slices.Concat(files, listed) {
		base := path.Base(f)
		if seen[base] || !feed.ShowPath(base) {
			continue
		}
		seen[base] = true
		if err := carryListeningState(ctx, tx, lib, JoinPart{Path: from + "/" + base, Last: true}, to+"/"+base, 0, showMoveCarry); err != nil {
			return fmt.Errorf("move episode listening state: %w", err)
		}
	}
	if !stale {
		return nil
	}
	return pruneShow(ctx, tx, lib, from)
}
