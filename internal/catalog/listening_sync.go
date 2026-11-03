package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"time"
)

// Sync v2 (Track W1, migration 0061): players post what they listened to as
// events (POST /me/listening/sync), each with its own id, and the server derives
// progress, listening sessions and history from them. An event is stored once
// per user (a replay is "duplicate"), its times are the device's corrected by
// the batch's clock shift, and its place resolves against the stored row by
// ResolvePlace (a place made without seeing the row is "diverged": the further
// one is kept and the other is offered, other_place). The legacy PUT
// .../progress keeps last-write-wins and is logged as a "put" event
// (SaveProgressFrom). See CONVENTIONS.md, Listening sync.

// Event kinds.
const (
	EventListen   = "listen"
	EventFinish   = "finish"
	EventUnfinish = "unfinish"
	eventPut      = "put"
)

// Event statuses (SyncResult.Status).
const (
	statusApplied    = "applied"
	statusDuplicate  = "duplicate"
	statusDiverged   = "diverged"
	statusNotApplied = "not_applied"
	statusRejected   = "rejected"
)

// Event reasons (SyncResult.Reason).
const (
	reasonOlder        = "older"
	reasonResumeBehind = "resume_behind"
	// reasonBehind: a furthest-only batch's (SyncBatch.FurthestOnly) place not
	// ahead of the stored one, or its unfinish: never applied, never offered.
	reasonBehind = "behind"
	// SyncInvalid, SyncNoAccess and SyncNotFound are also a SyncTarget's
	// reason: the handler decides them while resolving an event's path.
	SyncInvalid            = "invalid"
	reasonUnknownKind      = "unknown_kind"
	reasonTooOld           = "too_old"
	reasonDurationMismatch = "duration_mismatch"
	SyncNoAccess           = "no_access"
	SyncNotFound           = "not_found"
	// SyncNotSynced is a target's reason for an Audible account's batch (Bridge
	// Phase 4c): the book is none of the account's synced titles (unmatched,
	// "Not this book", "Don't sync this book", or gone from the account).
	SyncNotSynced = "not_synced"
)

// Sync limits and tolerances.
const (
	// MaxSyncEvents is the most events one batch may carry.
	MaxSyncEvents = 500
	// MaxClockOffset bounds a batch's clock_offset (seconds).
	MaxClockOffset = 1e9
	// MaxPathBytes is the longest path, as sent, a sync event or any
	// path-addressed route (the API's pathInScope) may name (server#289): a
	// longer one is no file's on Linux (PATH_MAX is 4096, and the library's
	// root comes first) or macOS (1024), and it would be stored as is on the
	// progress, event, session, history and bookmark rows.
	MaxPathBytes = 4096
	// syncRejectAge: an event older than this (corrected) is rejected too_old.
	syncRejectAge = 90 * 24 * time.Hour
	// SyncRetention is how long stored events are kept (PruneListeningEvents):
	// longer than syncRejectAge, so a pruned id replayed with a clock shift up to
	// 30 days different is still too old to apply twice.
	SyncRetention = 120 * 24 * time.Hour
	// maxSpan is the longest one listen span may be.
	maxSpan = 24 * time.Hour
	// fingerprintTolerance: a device's book length further than this from the
	// indexed book's is another edition (duration_mismatch).
	fingerprintTolerance = 10.0
	// resumeBehindSlack: a resume or continue span ending this far before the
	// stored place is not applied (resume_behind).
	resumeBehindSlack = 60.0
	// otherPlaceMin: a place within this of the kept one is not offered.
	otherPlaceMin = 30.0
	// furthestSlack: a furthest-only listen must be more than this ahead of
	// the stored place to apply (an Audible place the Bridge pushed there,
	// read back, is the same place, not a move).
	furthestSlack = 1.0
	// historyJoinGap and historyJoinOverlap: a span starting up to historyJoinGap
	// after a history row on the book ended (or up to historyJoinOverlap before
	// it: two batches' clock shifts differ by their latency) and within
	// historyJoinPos of where it ended extends that row. historyJoinPos is also
	// how close a resume_behind span must start to its device's offered place to
	// carry the offer on (resolveListen), and all three say whether a span
	// continues its device's chain from an earlier POST (continues).
	historyJoinGap     = 60 * time.Second
	historyJoinOverlap = 2 * time.Second
	historyJoinPos     = 5.0
)

var syncEventID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// listenStarts are a listen span's start kinds: how the span began. resume and
// continue never move the place back (resume_behind); the others are a
// deliberate move.
var listenStarts = map[string]bool{"resume": true, "continue": true, "moved_back": true, "jump": true, "restart": true}

// SyncEvent is one event of a batch as the player sends it. Listen spans carry
// whole-book positions (FromPos, ToPos) and optionally the file places
// (fe#188: the audio file's rel path and seconds within it) that the server
// prefers when the file is in the book; finish and unfinish carry only At.
type SyncEvent struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	LibraryID     int64    `json:"library_id"`
	Path          string   `json:"path"`
	FromPos       float64  `json:"from_pos"`
	ToPos         float64  `json:"to_pos"`
	FromFile      *string  `json:"from_file"`
	FromFilePos   *float64 `json:"from_file_pos"`
	ToFile        *string  `json:"to_file"`
	ToFilePos     *float64 `json:"to_file_pos"`
	Duration      float64  `json:"duration"`
	PlaybackSpeed float64  `json:"playback_speed"`
	StartedAt     string   `json:"started_at"`
	EndedAt       string   `json:"ended_at"`
	Start         string   `json:"start"`
	At            string   `json:"at"`
	BaseVersion   *int64   `json:"base_version"`
	// undecodable: DecodeSyncEvents could not read the event (its result is
	// "invalid"); never on the wire.
	undecodable bool
}

// DecodeSyncEvents reads a batch's events one by one, strictly (unknown fields
// refused, and a listen's from_pos and to_pos required), so a malformed event
// is that event's "invalid" and never fails the batch. An undecodable event
// keeps the id it can be read with (if any), for its result.
func DecodeSyncEvents(raw []json.RawMessage) []SyncEvent {
	out := make([]SyncEvent, len(raw))
	for i, r := range raw {
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out[i]); err == nil && (out[i].Kind != EventListen || sentPositions(r)) {
			continue
		}
		var id struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r, &id)
		out[i] = SyncEvent{ID: id.ID, undecodable: true}
	}
	return out
}

// sentPositions reports whether an event sent both from_pos and to_pos as
// numbers: a null or missing one decodes as 0, which a listen would apply as
// the place 0:00.
func sentPositions(r json.RawMessage) bool {
	var pos struct {
		FromPos *float64 `json:"from_pos"`
		ToPos   *float64 `json:"to_pos"`
	}
	return json.Unmarshal(r, &pos) == nil && pos.FromPos != nil && pos.ToPos != nil
}

// SyncFile is one audio file of an indexed book on its whole-book timeline.
type SyncFile struct {
	Path     string
	Offset   float64
	Duration float64
}

// SyncBook is an indexed book as a sync reads it: its length and its files in
// their current order (a single-file book is its own one file).
type SyncBook struct {
	Path     string
	Duration float64
	Files    []SyncFile
}

// SyncBookAt reads the indexed book at path (its duration and its files'
// offsets, the cumulative book_files durations by seq as GET .../chapters
// lists them), nil when no book is indexed there.
func (c *Catalog) SyncBookAt(ctx context.Context, libraryID int64, path string) (*SyncBook, error) {
	var book Book
	err := c.db.QueryRowContext(ctx,
		`SELECT id, rel_path, is_folder, size, duration FROM books WHERE library_id = ? AND rel_path = ?`,
		libraryID, path).Scan(&book.ID, &book.RelPath, &book.IsFolder, &book.Size, &book.Duration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files, err := playFiles(ctx, c.db, &book)
	if err != nil {
		return nil, err
	}
	b := SyncBook{Path: path, Duration: book.Duration, Files: make([]SyncFile, len(files))}
	offset := 0.0
	for i, f := range files {
		b.Files[i] = SyncFile{Path: f.RelPath, Offset: offset, Duration: f.Duration}
		offset += f.Duration
	}
	return &b, nil
}

// SyncTarget is what the handler resolved for one (library_id, path) a batch
// names: the progress key (the book's path, progressPath's rule), or the reason
// its events are rejected (SyncNoAccess, SyncNotFound, SyncInvalid).
type SyncTarget struct {
	Key    Ref
	Reason string
	// Exact: the sent path is the indexed book itself (no part or file mapping),
	// the only case whose duration is fingerprinted.
	Exact bool
	// Book is the indexed book at Key, nil when none is (the events apply
	// path-keyed with no fingerprint or file remap, as a PUT does).
	Book *SyncBook
	// Transcoding: the token streamed this book through the transcoder lately
	// (StreamMarks), for a span that is live.
	Transcoding bool
}

// SyncBatch is one POST /me/listening/sync: the device, the clock reference
// (exactly one of SentAt and ClockOffset) and the events, with every
// (library_id, path) they name resolved in Targets.
type SyncBatch struct {
	UserID     int64
	DeviceID   string
	TokenID    int64
	DeviceName string
	Client     Client
	// SentAt is the device's clock when it sent the batch; ClockOffset (seconds,
	// server minus device) is a relay's measured offset.
	SentAt      *time.Time
	ClockOffset *float64
	Events      []SyncEvent
	Targets     map[Ref]SyncTarget
	// FurthestOnly is an Audible account's batch (Bridge Phase 4c), set only by
	// the Bridge's route, never from a request body: a listen applies only
	// ahead of the stored place (furthestSlack), over any device's row and
	// whatever its time, else it is not_applied "behind" with no offer (a
	// finished row is at its end); an unfinish is "behind" too; a finish applies
	// as any finish. Its spans are a place only: no listening session, daily
	// roll-up or history is recorded (owner decision 2), and TokenID is 0.
	FurthestOnly bool
}

// SyncResult is one event's answer.
type SyncResult struct {
	ID         string               `json:"id"`
	Status     string               `json:"status"`
	Reason     string               `json:"reason,omitempty"`
	Kept       string               `json:"kept,omitempty"`
	OtherPlace *ListeningOtherPlace `json:"other_place,omitempty"`
}

// SyncAnswer is a batch's answer: the server's time, a result per event in
// request order, and the progress of every book a non-rejected event touched,
// read after the batch.
type SyncAnswer struct {
	ServerTime string       `json:"server_time"`
	Results    []SyncResult `json:"results"`
	Progress   []Progress   `json:"progress"`
}

// syncPrepared is an event that passed the checks, corrected and remapped,
// waiting for the transaction.
type syncPrepared struct {
	index          int
	ev             SyncEvent
	target         SyncTarget
	started, ended time.Time // corrected (finish/unfinish: both At)
	from, to       float64   // whole-book seconds after the remap
	speed          float64
}

// length is the span's book length: the device's, else the indexed book's
// (0 unknown).
func (p syncPrepared) length() float64 {
	if p.ev.Duration <= 0 && p.target.Book != nil {
		return p.target.Book.Duration
	}
	return p.ev.Duration
}

// SyncListening applies one batch in ONE writer transaction (normal
// durability, like SaveProgress): the checks first (a rejected event is never
// stored), then each event in corrected-time order (ties by request index),
// answering in request order. A failed transaction stores nothing and returns
// its error (the client retries the whole batch, which then applies once).
func (c *Catalog) SyncListening(ctx context.Context, b SyncBatch) (*SyncAnswer, error) {
	now := c.now()
	// The batch's clock shift as a Duration: sent_at's is the exact difference
	// (saturating at about 292 years, the same on every platform, where a float
	// of seconds converted back would overflow differently on amd64 and arm64);
	// clock_offset's is bounded by MaxClockOffset, so its conversion fits.
	var shiftDur time.Duration
	switch {
	case b.ClockOffset != nil:
		shiftDur = time.Duration(*b.ClockOffset * float64(time.Second))
	case b.SentAt != nil:
		shiftDur = now.Sub(*b.SentAt)
	}
	shift := shiftDur.Seconds()
	results := make([]SyncResult, len(b.Events))
	seen := map[string]bool{}
	var prepared []syncPrepared
	// touched: the books every non-rejected event names, in request order (no
	// event is rejected once it is prepared: the transaction never rejects).
	var touched []Ref
	touchedSet := map[Ref]bool{}
	touch := func(ev SyncEvent) {
		if t, ok := b.Targets[Ref{LibraryID: ev.LibraryID, Path: ev.Path}]; ok && t.Reason == "" && !touchedSet[t.Key] {
			touchedSet[t.Key] = true
			touched = append(touched, t.Key)
		}
	}
	for i, ev := range b.Events {
		results[i].ID = ev.ID
		reject := func(reason string) { results[i].Status, results[i].Reason = statusRejected, reason }
		if ev.undecodable || !syncEventID.MatchString(ev.ID) {
			reject(SyncInvalid)
			continue
		}
		// An id repeated in the batch is a duplicate of its first copy that
		// passed the checks; a copy rejected is not seen, so the next is checked
		// on its own (server#284: it was answered duplicate and nothing stored).
		if seen[ev.ID] {
			results[i].Status = statusDuplicate
			touch(ev)
			continue
		}
		if ev.Kind != EventListen && ev.Kind != EventFinish && ev.Kind != EventUnfinish {
			reject(reasonUnknownKind)
			continue
		}
		p, ok := prepareSyncEvent(ev)
		if !ok {
			reject(SyncInvalid)
			continue
		}
		target, ok := b.Targets[Ref{LibraryID: ev.LibraryID, Path: ev.Path}]
		switch {
		case !ok:
			reject(SyncInvalid)
			continue
		case target.Reason != "":
			reject(target.Reason)
			continue
		}
		p.index, p.target = i, target
		// The clock: every time of the batch moves by the shift; a corrected time
		// past now is pulled back to now, both ends of a span alike (by its own
		// length, at most maxSpan: the distance past now can saturate).
		p.started, p.ended = p.started.Add(shiftDur), p.ended.Add(shiftDur)
		if p.ended.After(now) {
			p.started, p.ended = now.Add(-p.ended.Sub(p.started)), now
		}
		if p.ended.Before(now.Add(-syncRejectAge)) {
			reject(reasonTooOld)
			continue
		}
		if book := target.Book; target.Exact && book != nil && ev.Duration > 0 && book.Duration > 0 &&
			math.Abs(ev.Duration-book.Duration) > fingerprintTolerance {
			reject(reasonDurationMismatch)
			continue
		}
		if ev.Kind == EventListen {
			p.from = remapEnd(target.Book, ev.FromPos, ev.FromFile, ev.FromFilePos)
			p.to = remapEnd(target.Book, ev.ToPos, ev.ToFile, ev.ToFilePos)
		}
		seen[ev.ID] = true
		prepared = append(prepared, p)
		touch(ev)
	}
	sort.SliceStable(prepared, func(i, j int) bool {
		if !prepared[i].ended.Equal(prepared[j].ended) {
			return prepared[i].ended.Before(prepared[j].ended)
		}
		return prepared[i].index < prepared[j].index
	})
	// A batch with nothing to apply (empty, or every event rejected or a
	// duplicate of an earlier copy) opens no writer transaction.
	var err error
	if len(prepared) > 0 {
		err = c.db.WithUserStateTx(ctx, "SyncListening", func(tx *sql.Tx) error {
			s := syncRun{c: c, tx: tx, b: b, now: now, shift: shift, written: map[Ref]int64{}}
			var err error
			if s.stored, err = storedEventIDs(ctx, tx, b.UserID, prepared); err != nil {
				return err
			}
			for _, p := range prepared {
				res, err := s.apply(ctx, p)
				if err != nil {
					return err
				}
				res.ID = p.ev.ID
				results[p.index] = res
			}
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	answer := &SyncAnswer{ServerTime: formatSessionTime(now), Results: results, Progress: []Progress{}}
	for _, key := range touched {
		p, err := getProgress(ctx, c.db, b.UserID, key)
		if err != nil {
			return nil, err
		}
		if p != nil {
			answer.Progress = append(answer.Progress, *p)
		}
	}
	return answer, nil
}

// prepareSyncEvent checks an event's fields (a known kind) and parses its
// times, false for an invalid one: library_id > 0 and a path of at most
// MaxPathBytes; a listen's
// positions finite and >= 0, RFC3339 times with ended >= started at most 24 h
// apart, a known start, file places both-or-neither with a seconds >= 0, a
// duration >= 0 and a speed in (0, 4] (0 = 1); a finish's or unfinish's at in
// RFC3339; a base version >= 0 when sent.
func prepareSyncEvent(ev SyncEvent) (syncPrepared, bool) {
	p := syncPrepared{ev: ev, speed: 1}
	if ev.LibraryID <= 0 || ev.Path == "" || len(ev.Path) > MaxPathBytes || (ev.BaseVersion != nil && *ev.BaseVersion < 0) {
		return p, false
	}
	if ev.Kind != EventListen {
		at, err := time.Parse(time.RFC3339, ev.At)
		if err != nil {
			return p, false
		}
		p.started, p.ended = at, at
		return p, true
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
	filePair := func(file *string, pos *float64) bool {
		return (file == nil) == (pos == nil) && (pos == nil || (*file != "" && finite(*pos)))
	}
	started, err1 := time.Parse(time.RFC3339, ev.StartedAt)
	ended, err2 := time.Parse(time.RFC3339, ev.EndedAt)
	switch {
	case err1 != nil || err2 != nil || ended.Before(started) || ended.Sub(started) > maxSpan:
		return p, false
	case !finite(ev.FromPos) || !finite(ev.ToPos) || !finite(ev.Duration):
		return p, false
	case !listenStarts[ev.Start]:
		return p, false
	case !filePair(ev.FromFile, ev.FromFilePos) || !filePair(ev.ToFile, ev.ToFilePos):
		return p, false
	case math.IsNaN(ev.PlaybackSpeed) || ev.PlaybackSpeed < 0 || ev.PlaybackSpeed > 4:
		return p, false
	}
	if ev.PlaybackSpeed > 0 {
		p.speed = ev.PlaybackSpeed
	}
	p.started, p.ended = started, ended
	return p, true
}

// remapEnd is one end of a span on the book's current timeline (fe#188): a
// file place whose file is one of the book's is that file's offset now plus the
// seconds in it (at most its length); anything else is the sent whole-book
// seconds.
func remapEnd(book *SyncBook, whole float64, file *string, pos *float64) float64 {
	if book == nil || file == nil || pos == nil {
		return whole
	}
	clean := CleanRelPath(*file)
	for _, f := range book.Files {
		if f.Path == clean {
			in := *pos
			if f.Duration > 0 {
				in = math.Min(in, f.Duration)
			}
			return f.Offset + in
		}
	}
	return whole
}

// syncRun is one batch's transaction.
type syncRun struct {
	c     *Catalog
	tx    *sql.Tx
	b     SyncBatch
	now   time.Time
	shift float64
	// written is the version each book's row had after this batch wrote it, or
	// the row's own version when the batch carries on from this device's earlier
	// POST (a replay of the newest link of its chain, or a span continuing that
	// chain: continues): a base-0 span resolves on it.
	written map[Ref]int64
	// stored is the batch's event ids already in the log (a replay: duplicate),
	// each with the end it was stored with (ended_at, corrected then).
	stored map[string]string
}

// storedEventIDs is which of the prepared events' ids the user's log already
// holds, each with its stored ended_at, read in one statement (a batch is at
// most MaxSyncEvents ids).
func storedEventIDs(ctx context.Context, tx *sql.Tx, userID int64, prepared []syncPrepared) (map[string]string, error) {
	out := map[string]string{}
	if len(prepared) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(prepared)+1)
	args = append(args, userID)
	for _, p := range prepared {
		args = append(args, p.ev.ID)
	}
	type storedEvent struct{ id, ended string }
	evs, err := queryRows(ctx, tx, func(rows *sql.Rows, e *storedEvent) error { return rows.Scan(&e.id, &e.ended) },
		`SELECT event_id, ended_at FROM listening_events WHERE user_id = ? AND event_id IN (`+placeholders(len(prepared))+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		out[e.id] = e.ended
	}
	return out, nil
}

// syncRow is the stored progress row as a sync reads it.
type syncRow struct {
	position, duration float64
	finished           bool
	version            int64
	deviceID           string
	deviceName         string
	updatedAt          string
	finishChangedAt    sql.NullString
	// The offered place's position, device and version (all null for none).
	otherPosition sql.NullFloat64
	otherDeviceID sql.NullString
	otherVersion  sql.NullInt64
	// spanAt is the end of the span the row's device last applied (or of a
	// finish or unfinish of its own after it), null unless recorded at the row's
	// current version (0062): that device's chain (chainOf).
	spanAt sql.NullString
}

func (s *syncRun) row(ctx context.Context, key Ref) (*syncRow, error) {
	var r syncRow
	err := s.tx.QueryRowContext(ctx,
		`SELECT position, duration, finished, version, device_id, device_name, updated_at, finish_changed_at,
		        other_position, other_device_id, CASE WHEN other_position IS NULL THEN NULL ELSE other_version END,
		        CASE WHEN span_version = version THEN span_at END
		   FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		s.b.UserID, key.LibraryID, key.Path).
		Scan(&r.position, &r.duration, &r.finished, &r.version, &r.deviceID, &r.deviceName, &r.updatedAt,
			&r.finishChangedAt, &r.otherPosition, &r.otherDeviceID, &r.otherVersion, &r.spanAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// apply runs one prepared event: the dedupe, then its kind's rule, then its
// row in the log.
func (s *syncRun) apply(ctx context.Context, p syncPrepared) (SyncResult, error) {
	if ended, replay := s.stored[p.ev.ID]; replay {
		// A replay (the post that stored it lost its answer, so the device sends
		// it again ahead of what it recorded since): when it is the newest link
		// of this device's chain on the row (chainOf, ending at its end: no edit,
		// PUT or other writer since, and it wrote the row, not a span that
		// diverged and kept the stored place; a finish or unfinish by its
		// finish_changed_at, as a late one keeps the chain's later end), the
		// device's later events on base 0 chain on it, as they would have in
		// that post.
		if _, ok := s.written[p.target.Key]; !ok {
			stored, err := s.row(ctx, p.target.Key)
			if err != nil {
				return SyncResult{}, err
			}
			if at, chained := s.chainOf(stored); chained &&
				(at == ended || p.ev.Kind != EventListen && stored.finishChangedAt.String == ended) {
				s.written[p.target.Key] = stored.version
			}
		}
		return SyncResult{Status: statusDuplicate}, nil
	}
	var (
		res SyncResult
		err error
	)
	if p.ev.Kind == EventListen {
		res, err = s.listen(ctx, p)
	} else {
		res, err = s.finish(ctx, p)
	}
	if err != nil {
		return res, err
	}
	return res, insertEvent(ctx, s.tx, eventRow{
		userID: s.b.UserID, id: p.ev.ID, kind: p.ev.Kind, ref: p.target.Key, deviceID: s.b.DeviceID,
		tokenID: s.b.TokenID, from: p.from, to: p.to, speed: p.speed, duration: p.ev.Duration,
		started: formatSessionTime(p.started), ended: formatSessionTime(p.ended), shift: s.shift,
		start: p.ev.Start, base: p.ev.BaseVersion, status: res.Status, reason: res.Reason,
		received: formatSessionTime(s.now),
	})
}

// listen resolves a span's place against the stored row, then records its
// listening (sessions, history) whatever the place did.
func (s *syncRun) listen(ctx context.Context, p syncPrepared) (SyncResult, error) {
	key := p.target.Key
	stored, err := s.row(ctx, key)
	if err != nil {
		return SyncResult{}, err
	}
	at := formatSessionTime(p.ended)
	mine := &ListeningOtherPlace{Position: p.to, DeviceID: s.b.DeviceID, DeviceName: s.b.DeviceName, UpdatedAt: at}
	if s.b.FurthestOnly {
		return s.furthestListen(ctx, p, stored, mine.UpdatedAt)
	}
	res, err := s.resolveListen(ctx, p, stored, mine)
	if err != nil {
		return res, err
	}
	return res, s.recordListening(ctx, p)
}

// furthestListen is a furthest-only batch's listen (SyncBatch.FurthestOnly):
// behind (or at) the stored place, or on a finished row, it is not applied and
// offers nothing; ahead it is written whatever device or time the row has
// (the player's own reconcile then moves or offers), the row's time never
// moving back and its offered place kept. No listening is recorded.
func (s *syncRun) furthestListen(ctx context.Context, p syncPrepared, stored *syncRow, at string) (SyncResult, error) {
	version := int64(1)
	if stored != nil {
		if stored.finished || p.to <= stored.position+furthestSlack {
			return SyncResult{Status: statusNotApplied, Reason: reasonBehind}, nil
		}
		version = stored.version + 1
		at = laterStamp(at, stored.updatedAt)
	}
	if err := s.writePlace(ctx, p, version, false, at); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Status: statusApplied}, nil
}

// resolveListen is a span's place: ResolvePlace, with the backstop, and the
// write they decide. The backstop: a resume, or its continue-chain, never moves
// the place back by more than resumeBehindSlack (a deliberate move is
// moved_back, jump or restart), so a device that started playing before it
// had the stored place (a cold start, base 0 or none) can't pull the book back
// to where it began. A span that diverged from a place its device had seen
// (base > 0, another device wrote the row since) is not a cold start: it was
// real listening elsewhere, and ResolvePlace keeps the further place and offers
// (and persists) the span's, which the backstop would drop.
func (s *syncRun) resolveListen(ctx context.Context, p syncPrepared, stored *syncRow, mine *ListeningOtherPlace) (SyncResult, error) {
	key := p.target.Key
	chainAt, chained := s.chainOf(stored)
	// A span carrying on from this device's chain of an earlier POST resolves on
	// the row's version (server#289), as a base-0 span after this batch's own
	// write does; each span is judged on its own until one does.
	if _, ok := s.written[key]; !ok && chained && continues(p, chainAt, stored.position) {
		s.written[key] = stored.version
	}
	base := p.ev.BaseVersion
	// In-batch chaining: a device's first spans on a book in one POST share base
	// 0; the later ones resolve on what the earlier wrote, not against it (and
	// a continuation in a later POST on what that POST wrote, above).
	if v, ok := s.written[key]; ok && base != nil && *base == 0 {
		base = &v
	}
	in := Place{Progression: p.to, DeviceID: s.b.DeviceID, UpdatedAt: mine.UpdatedAt, Version: 1}
	var storedPlace *Place
	if stored != nil {
		in.Version = stored.version + 1
		storedPlace = &Place{Progression: stored.position, DeviceID: stored.deviceID,
			UpdatedAt: stored.updatedAt, Version: stored.version}
		if chained {
			storedPlace.UpdatedAt = chainAt
		}
	}
	outcome := ResolvePlace(storedPlace, in, base)
	divergedFromSeen := (outcome == PlaceKeepStored || outcome == PlaceApplyDiverged) && base != nil && *base > 0
	if (p.ev.Start == "resume" || p.ev.Start == "continue") && stored != nil && !stored.finished &&
		p.to < stored.position-resumeBehindSlack && !divergedFromSeen {
		// A span carrying on from this device's offered place (a jump behind the
		// row, then its continue chain) takes the offer with it, so the place
		// offered is where the device got to, not where it began.
		if stored.otherPosition.Valid && stored.otherDeviceID.String == s.b.DeviceID &&
			math.Abs(p.from-stored.otherPosition.Float64) <= historyJoinPos {
			if err := s.setOther(ctx, key, mine, stored.version+1); err != nil {
				return SyncResult{}, err
			}
		}
		return SyncResult{Status: statusNotApplied, Reason: reasonResumeBehind, OtherPlace: mine}, nil
	}
	// The row's time never moves back (server#284): a span that ended before the
	// row it replaces keeps the row's updated_at, so an old app's PUT dated in
	// between can't win last-write-wins against it (and a span chaining on a
	// diverged apply of its device's, span_at, keeps its kept time too).
	rowAt := mine.UpdatedAt
	if stored != nil {
		rowAt = laterStamp(rowAt, stored.updatedAt)
	}
	switch outcome {
	case PlaceStale:
		return SyncResult{Status: statusNotApplied, Reason: reasonOlder}, nil
	case PlaceKeepStored:
		// The stored row stays; the span's place is offered (and kept on the row,
		// unless it is the same device's or about the same place) at the row's
		// next version: the row's own device acknowledged the current one before
		// the offer existed, so a base of it must not clear an offer that device
		// never received.
		if s.b.DeviceID != stored.deviceID && math.Abs(p.to-stored.position) > otherPlaceMin {
			if err := s.setOther(ctx, key, mine, stored.version+1); err != nil {
				return SyncResult{}, err
			}
		}
		return SyncResult{Status: statusDiverged, Kept: "stored", OtherPlace: mine}, nil
	case PlaceApplyDiverged:
		lost := &ListeningOtherPlace{Position: stored.position, DeviceID: stored.deviceID,
			DeviceName: stored.deviceName, UpdatedAt: stored.updatedAt}
		if err := s.writePlace(ctx, p, in.Version, false, rowAt); err != nil {
			return SyncResult{}, err
		}
		if stored.deviceID != s.b.DeviceID && math.Abs(p.to-stored.position) > otherPlaceMin {
			if err := s.setOther(ctx, key, lost, in.Version); err != nil {
				return SyncResult{}, err
			}
		}
		return SyncResult{Status: statusDiverged, Kept: "event", OtherPlace: lost}, nil
	}
	// Applied: an offered place is cleared by a device that had seen it: its base
	// as sent (never the in-batch chaining's, which the device didn't see) at or
	// after the version it was recorded at.
	sent := p.ev.BaseVersion
	clear := stored != nil && stored.otherVersion.Valid && sent != nil && *sent >= stored.otherVersion.Int64
	if err := s.writePlace(ctx, p, in.Version, clear, rowAt); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Status: statusApplied}, nil
}

// writePlace stores a span's place at version: position, length (the span's,
// else the book's, else kept), speed, device, updatedAt and version, a start
// when none; never the finish state. clear also drops the offered place. The
// span's end is the newest link of the device's chain, kept on the row at
// version (span_at, span_version: chainOf) for its next span, in this POST or a
// later one.
func (s *syncRun) writePlace(ctx context.Context, p syncPrepared, version int64, clear bool, updatedAt string) error {
	key := p.target.Key
	spanAt := formatSessionTime(p.ended)
	if _, err := s.tx.ExecContext(ctx,
		`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed,
		     version, device_id, device_name, updated_at, started_at, span_at, span_version)
		 VALUES(?1,?2,?3,?4,?5,0,?6,?7,?8,?9,?10,?11,?13,?14)
		 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
		     position=excluded.position,
		     duration=CASE WHEN excluded.duration > 0 THEN excluded.duration ELSE progress.duration END,
		     playback_speed=excluded.playback_speed, version=excluded.version, device_id=excluded.device_id,
		     device_name=excluded.device_name, updated_at=excluded.updated_at,
		     started_at=COALESCE(progress.started_at, excluded.started_at),
		     other_position=CASE WHEN ?12 THEN NULL ELSE progress.other_position END,
		     other_device_id=CASE WHEN ?12 THEN NULL ELSE progress.other_device_id END,
		     other_device_name=CASE WHEN ?12 THEN NULL ELSE progress.other_device_name END,
		     other_at=CASE WHEN ?12 THEN NULL ELSE progress.other_at END,
		     other_version=CASE WHEN ?12 THEN NULL ELSE progress.other_version END,
		     span_at=excluded.span_at, span_version=excluded.span_version`,
		s.b.UserID, key.LibraryID, key.Path, p.to, p.length(), p.speed, version, s.b.DeviceID, s.b.DeviceName,
		updatedAt, p.started.UTC().Format(time.RFC3339), clear, spanAt, version); err != nil {
		return err
	}
	s.written[key] = version
	return nil
}

// chainOf is this device's span chain on the row (server#289, 0062): the end of
// the span it last applied there, or of a finish or unfinish of its own after
// it (span_at), while the row is still as that write left it (this device's, at
// span_version; every other writer ends it: endSpanChain). The device's next
// span compares with that end, not with the row's updated_at that a diverged
// apply kept later (writePlace), whether it comes in this POST or a later one,
// so a backlog split across POSTs (the player halving a batch on 413, an outbox
// longer than one batch) is compared as one POST would be; its base-0 spans
// chain only from the first that continues (continues), where one POST would
// have chained every span after its first write.
func (s *syncRun) chainOf(stored *syncRow) (string, bool) {
	if stored == nil || stored.deviceID != s.b.DeviceID || !stored.spanAt.Valid {
		return "", false
	}
	return stored.spanAt.String, true
}

// continues reports whether listen span p carries on from a chain ending at end
// on a row at position: it starts where (historyJoinPos) or when
// (historyJoinGap, historyJoinOverlap before) the chain ended. A base-0 span
// that does resolves on the row's version, as a later base-0 span of the same
// POST does; one that doesn't is still diverged from the device's own place (a
// device signed out and back in).
func continues(p syncPrepared, end string, position float64) bool {
	at := parseSessionTime(end)
	after := !p.started.Before(at.Add(-historyJoinOverlap))
	when := after && !p.started.After(at.Add(historyJoinGap))
	where := after && math.Abs(p.from-position) <= historyJoinPos
	return when || where
}

// laterStamp is the later of a span's end (fixed-width) and a stored row's
// updated_at, which a legacy PUT may have written in any RFC3339 form; the
// span's when the stored one doesn't parse.
func laterStamp(span, stored string) string {
	if t, err := time.Parse(time.RFC3339Nano, stored); err == nil && t.After(parseSessionTime(span)) {
		return stored
	}
	return span
}

// setOther keeps place on the row as its offered place, recorded at version.
func (s *syncRun) setOther(ctx context.Context, key Ref, place *ListeningOtherPlace, version int64) error {
	_, err := s.tx.ExecContext(ctx,
		`UPDATE progress SET other_position = ?, other_device_id = ?, other_device_name = ?, other_at = ?,
		     other_version = ?
		  WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		place.Position, place.DeviceID, place.DeviceName, place.UpdatedAt, version,
		s.b.UserID, key.LibraryID, key.Path)
	return err
}

// finish applies a finish or unfinish newer than the row's last finish-state
// change (else "older"): finish marks the book finished (dated at, to the
// second, unless it already was), moves the place to the end when the length
// is known and drops the offered place (the listener chose the end, as Mark
// finished does); unfinish clears the finish and keeps the place, and takes a
// finished row for this device (so the restart span that follows it resolves
// on this device's own row, not as diverged from the device that finished
// it). Both bump the version and record at as finish_changed_at; the base is
// ignored. A change that leaves the row this device's is its chain's newest
// link (span_at, chainOf), in this POST and the next, unless the chain already
// ends later or the row's time is later than the change; an unfinish of
// another device's unfinished row takes nothing, so this device's next base-0
// span still diverges from that row. An unfinish with no row has nothing to
// change (applied, nothing written).
func (s *syncRun) finish(ctx context.Context, p syncPrepared) (SyncResult, error) {
	key := p.target.Key
	if s.b.FurthestOnly && p.ev.Kind == EventUnfinish {
		return SyncResult{Status: statusNotApplied, Reason: reasonBehind}, nil
	}
	stored, err := s.row(ctx, key)
	if err != nil {
		return SyncResult{}, err
	}
	if stored != nil && stored.finishChangedAt.Valid {
		if changed := parseSessionTime(stored.finishChangedAt.String); !p.ended.After(changed) {
			return SyncResult{Status: statusNotApplied, Reason: reasonOlder}, nil
		}
	}
	at := formatSessionTime(p.ended)
	day := p.ended.UTC().Format(time.RFC3339)
	if stored == nil {
		if p.ev.Kind == EventUnfinish {
			return SyncResult{Status: statusApplied}, nil
		}
		duration := 0.0
		if p.target.Book != nil {
			duration = p.target.Book.Duration
		}
		if _, err := s.tx.ExecContext(ctx,
			`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed,
			     version, device_id, device_name, updated_at, started_at, finished_at, finish_changed_at,
			     span_at, span_version)
			 VALUES(?,?,?,?,?,1,1,1,?,?,?,?,?,?,?,1)`,
			s.b.UserID, key.LibraryID, key.Path, duration, duration, s.b.DeviceID, s.b.DeviceName, at, day, day,
			at, at); err != nil {
			return SyncResult{}, err
		}
		s.written[key] = 1
		return SyncResult{Status: statusApplied}, nil
	}
	// The row's time never moves back (a later save elsewhere stays later).
	updated := laterStamp(at, stored.updatedAt)
	version := stored.version + 1
	// takes: the row is this device's after the change (a finish takes it, an
	// unfinish a finished row, and its own stays its own). The change then moves
	// the device's chain here (span_at at the new version): its next span, in
	// this POST or a later one, compares with it, not the row's kept time (a
	// finish, an unfinish, then the restart that follows them), and a base-0 one
	// resolves on this version. Else the row stays another device's: its chain
	// ends (NULL) and this device's base-0 spans still diverge from it.
	takes := p.ev.Kind == EventFinish || stored.finished || stored.deviceID == s.b.DeviceID
	var chainAt, chainVersion any
	if takes {
		// The chain never moves back (a late batch's earlier finish keeps the
		// end of the device's later span) and never starts before the row's own
		// time (a save elsewhere since stays newer than the device's next span,
		// which compares with updated_at as it would without a chain).
		if end, chained := s.chainOf(stored); chained {
			chainAt, chainVersion = laterStamp(at, end), version
		} else if updated == at {
			chainAt, chainVersion = at, version
		}
	}
	if p.ev.Kind == EventFinish {
		duration := stored.duration
		if duration <= 0 && p.target.Book != nil {
			duration = p.target.Book.Duration
		}
		_, err = s.tx.ExecContext(ctx,
			`UPDATE progress SET finished = 1, finished_at = CASE WHEN finished THEN finished_at ELSE ?1 END,
			     position = CASE WHEN ?2 > 0 THEN ?2 ELSE position END,
			     duration = CASE WHEN ?2 > 0 THEN ?2 ELSE duration END,
			     version = ?3, device_id = ?4, device_name = ?5, updated_at = ?6, finish_changed_at = ?7,
			     span_at = ?11, span_version = ?12,
			     `+clearOtherPlace+`
			  WHERE user_id = ?8 AND library_id = ?9 AND rel_path = ?10`,
			day, duration, version, s.b.DeviceID, s.b.DeviceName, updated, at,
			s.b.UserID, key.LibraryID, key.Path, chainAt, chainVersion)
	} else {
		device, name := stored.deviceID, stored.deviceName
		if stored.finished {
			device, name = s.b.DeviceID, s.b.DeviceName
		}
		_, err = s.tx.ExecContext(ctx,
			`UPDATE progress SET finished = 0, finished_at = NULL, version = ?, device_id = ?, device_name = ?,
			     updated_at = ?, finish_changed_at = ?,
			     span_at = ?, span_version = ?
			  WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
			version, device, name, updated, at, chainAt, chainVersion, s.b.UserID, key.LibraryID, key.Path)
	}
	if err != nil {
		return SyncResult{}, err
	}
	if takes {
		s.written[key] = version
	}
	return SyncResult{Status: statusApplied}, nil
}

// recordListening counts a span's listening (every stored listen, whatever its
// place did): the token's session and the user's history, at the span's
// corrected times.
func (s *syncRun) recordListening(ctx context.Context, p syncPrepared) error {
	listened := listenedBetween(p.ended.Sub(p.started), p.to-p.from, p.speed)
	if err := s.recordSpan(ctx, p, listened); err != nil {
		return err
	}
	if listened < 1 {
		return nil
	}
	return s.recordHistory(ctx, p)
}

// recordSpan folds a span into the token's session on the book, as
// RecordHeartbeat folds a save, with the span's own times instead of the
// server's: it extends the token's newest session on the book that began by the
// span's start (latestSession) when the span starts within SessionGap of its
// last save (or later, playback having run on: continuousPlayback, within
// resumeWindow), else opens one at the span's start. Its listening is
// listenedBetween's clip of the span (a seek ahead, or a span longer than its
// wall time, is not listening). Only a span ending within livePlaying of now can
// be marked transcoded.
func (s *syncRun) recordSpan(ctx context.Context, p syncPrepared, listened float64) error {
	hb := Heartbeat{UserID: s.b.UserID, Ref: p.target.Key, TokenID: s.b.TokenID, DeviceName: s.b.DeviceName,
		Client: s.b.Client, Position: p.to, Duration: p.length(), Speed: p.speed,
		Transcoded: p.target.Transcoding && s.now.Sub(p.ended) <= livePlaying}
	open, err := latestSession(ctx, s.tx, hb, p.started)
	if err != nil {
		return err
	}
	if open != nil {
		gap := p.started.Sub(parseSessionTime(open.lastAt))
		if gap > SessionGap && !continuousPlayback(gap, p.from-open.endPos, p.speed) {
			open = nil
		}
	}
	ended := formatSessionTime(p.ended)
	if open == nil {
		return insertSession(ctx, s.tx, hb, formatSessionTime(p.started), ended, p.from, p.to, listened)
	}
	return extendSession(ctx, s.tx, open.id, hb, ended, p.to, listened)
}

// recordHistory adds a span to the user's listening history: it extends a row
// on the book (one recorded here, not imported) that ended just before the
// span began (historyJoinGap, or up to historyJoinOverlap after) where it
// begins (historyJoinPos), the newest such, so a run of 15 s continue spans is
// one row, a late backlog's too when another device's row on the book is newer;
// otherwise it adds a row of its own. The lookup is a range on
// idx_history_user_path_ended (0061), history times being fixed-width text.
func (s *syncRun) recordHistory(ctx context.Context, p syncPrepared) error {
	key := p.target.Key
	ended := formatSessionTime(p.ended)
	var id int64
	err := s.tx.QueryRowContext(ctx,
		`SELECT id FROM listening_history
		  WHERE user_id = ? AND library_id = ? AND rel_path = ? AND import_id = 0
		    AND ended_at BETWEEN ? AND ? AND ABS(to_pos - ?) <= ?
		  ORDER BY ended_at DESC, id DESC LIMIT 1`,
		s.b.UserID, key.LibraryID, key.Path, formatSessionTime(p.started.Add(-historyJoinGap)),
		formatSessionTime(p.started.Add(historyJoinOverlap)), p.from, historyJoinPos).Scan(&id)
	switch {
	case err == nil:
		_, err = s.tx.ExecContext(ctx,
			`UPDATE listening_history SET to_pos = ?, ended_at = MAX(ended_at, ?) WHERE id = ?`, p.to, ended, id)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = s.tx.ExecContext(ctx,
		`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at)
		 VALUES(?,?,?,?,?,?,?)`, s.b.UserID, key.LibraryID, key.Path, p.from, p.to, formatSessionTime(p.started), ended)
	return err
}

// eventRow is one listening_events row: a sync event after its checks, or a
// legacy PUT's (putEvent).
type eventRow struct {
	userID                    int64
	id, kind                  string
	ref                       Ref
	deviceID                  string
	tokenID                   int64
	from, to, speed, duration float64
	started, ended            string
	shift                     float64
	start                     string
	base                      *int64
	status, reason, received  string
}

// insertEvent logs r in tx (an id already stored is left as it is).
func insertEvent(ctx context.Context, tx *sql.Tx, r eventRow) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO listening_events(user_id, event_id, library_id, rel_path, kind, device_id, token_id,
		     from_pos, to_pos, speed, duration, started_at, ended_at, clock_shift, start, base_version,
		     status, reason, received_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id, event_id) DO NOTHING`,
		r.userID, r.id, r.ref.LibraryID, r.ref.Path, r.kind, r.deviceID, r.tokenID,
		r.from, r.to, r.speed, r.duration, r.started, r.ended, r.shift, r.start, r.base,
		r.status, r.reason, r.received)
	return err
}

// putEvent is a legacy PUT .../progress of in by userID's token tokenID as a
// listening event (SaveProgressFrom): a zero-length span at in's place, at at
// (its updated_at, fixed width), received now; the caller sets its status.
func putEvent(userID int64, in Progress, tokenID int64, at, received string) eventRow {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return eventRow{userID: userID, id: "put-" + hex.EncodeToString(b[:]), kind: eventPut, ref: in.Ref,
		deviceID: in.DeviceID, tokenID: tokenID, from: in.Position, to: in.Position, speed: in.PlaybackSpeed,
		duration: in.Duration, started: at, ended: at, received: received}
}

// PruneListeningEvents deletes the stored events that ended before cutoff, a
// batch at a time, and returns how many it deleted. The launcher runs it at
// startup and daily beside PruneSessions, at now - SyncRetention.
func (c *Catalog) PruneListeningEvents(ctx context.Context, cutoff time.Time) (int, error) {
	const batch = 5000
	total := 0
	for {
		res, err := c.db.ExecContext(ctx,
			`DELETE FROM listening_events WHERE id IN
			   (SELECT id FROM listening_events WHERE ended_at < ? LIMIT ?)`, formatSessionTime(cutoff), batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < batch {
			return total, nil
		}
	}
}
