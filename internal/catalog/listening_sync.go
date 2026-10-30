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
	// SyncInvalid, SyncNoAccess and SyncNotFound are also a SyncTarget's
	// reason: the handler decides them while resolving an event's path.
	SyncInvalid            = "invalid"
	reasonUnknownKind      = "unknown_kind"
	reasonTooOld           = "too_old"
	reasonDurationMismatch = "duration_mismatch"
	SyncNoAccess           = "no_access"
	SyncNotFound           = "not_found"
)

// Sync limits and tolerances.
const (
	// MaxSyncEvents is the most events one batch may carry.
	MaxSyncEvents = 500
	// MaxClockOffset bounds a batch's clock_offset (seconds).
	MaxClockOffset = 1e9
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
	// historyJoinGap and historyJoinOverlap: a span starting up to historyJoinGap
	// after the newest history row ended (or up to historyJoinOverlap before it:
	// two batches' clock shifts differ by their latency) and within
	// historyJoinPos of where it ended extends that row.
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
// refused), so a malformed event is that event's "invalid" and never fails the
// batch. An undecodable event keeps the id it can be read with (if any), for
// its result.
func DecodeSyncEvents(raw []json.RawMessage) []SyncEvent {
	out := make([]SyncEvent, len(raw))
	for i, r := range raw {
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out[i]); err == nil {
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
	shift := 0.0
	switch {
	case b.ClockOffset != nil:
		shift = *b.ClockOffset
	case b.SentAt != nil:
		shift = now.Sub(*b.SentAt).Seconds()
	}
	shiftDur := time.Duration(shift * float64(time.Second))
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
		if seen[ev.ID] {
			results[i].Status = statusDuplicate
			touch(ev)
			continue
		}
		seen[ev.ID] = true
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
		// past now is pulled back to now, both ends of a span alike.
		p.started, p.ended = p.started.Add(shiftDur), p.ended.Add(shiftDur)
		if over := p.ended.Sub(now); over > 0 {
			p.started, p.ended = p.started.Add(-over), now
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
		prepared = append(prepared, p)
		touch(ev)
	}
	sort.SliceStable(prepared, func(i, j int) bool {
		if !prepared[i].ended.Equal(prepared[j].ended) {
			return prepared[i].ended.Before(prepared[j].ended)
		}
		return prepared[i].index < prepared[j].index
	})
	err := c.db.WithUserStateTx(ctx, "SyncListening", func(tx *sql.Tx) error {
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
// times, false for an invalid one: library_id > 0 and a path; a listen's
// positions finite and >= 0, RFC3339 times with ended >= started at most 24 h
// apart, a known start, file places both-or-neither with a seconds >= 0, a
// duration >= 0 and a speed in (0, 4] (0 = 1); a finish's or unfinish's at in
// RFC3339; a base version >= 0 when sent.
func prepareSyncEvent(ev SyncEvent) (syncPrepared, bool) {
	p := syncPrepared{ev: ev, speed: 1}
	if ev.LibraryID <= 0 || ev.Path == "" || (ev.BaseVersion != nil && *ev.BaseVersion < 0) {
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
	// written is the version each book's row had after this batch wrote it.
	written map[Ref]int64
	// stored is the batch's event ids already in the log (a replay: duplicate).
	stored map[string]bool
}

// storedEventIDs is which of the prepared events' ids the user's log already
// holds, read in one statement (a batch is at most MaxSyncEvents ids).
func storedEventIDs(ctx context.Context, tx *sql.Tx, userID int64, prepared []syncPrepared) (map[string]bool, error) {
	out := map[string]bool{}
	if len(prepared) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(prepared)+1)
	args = append(args, userID)
	for _, p := range prepared {
		args = append(args, p.ev.ID)
	}
	ids, err := queryRows(ctx, tx, func(rows *sql.Rows, id *string) error { return rows.Scan(id) },
		`SELECT event_id FROM listening_events WHERE user_id = ? AND event_id IN (`+placeholders(len(prepared))+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
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
	otherVersion       sql.NullInt64
}

func (s *syncRun) row(ctx context.Context, key Ref) (*syncRow, error) {
	var r syncRow
	err := s.tx.QueryRowContext(ctx,
		`SELECT position, duration, finished, version, device_id, device_name, updated_at, finish_changed_at,
		        CASE WHEN other_position IS NULL THEN NULL ELSE other_version END
		   FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		s.b.UserID, key.LibraryID, key.Path).
		Scan(&r.position, &r.duration, &r.finished, &r.version, &r.deviceID, &r.deviceName, &r.updatedAt,
			&r.finishChangedAt, &r.otherVersion)
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
	if s.stored[p.ev.ID] {
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
	res, err := s.resolveListen(ctx, p, stored, mine)
	if err != nil {
		return res, err
	}
	return res, s.recordListening(ctx, p)
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
	base := p.ev.BaseVersion
	// In-batch chaining: a device's first spans on a book in one POST share base
	// 0; the later ones resolve on what the earlier wrote, not against it.
	if v, ok := s.written[key]; ok && base != nil && *base == 0 {
		base = &v
	}
	in := Place{Progression: p.to, DeviceID: s.b.DeviceID, UpdatedAt: mine.UpdatedAt, Version: 1}
	var storedPlace *Place
	if stored != nil {
		in.Version = stored.version + 1
		storedPlace = &Place{Progression: stored.position, DeviceID: stored.deviceID,
			UpdatedAt: stored.updatedAt, Version: stored.version}
	}
	outcome := ResolvePlace(storedPlace, in, base)
	divergedFromSeen := (outcome == PlaceKeepStored || outcome == PlaceApplyDiverged) && base != nil && *base > 0
	if (p.ev.Start == "resume" || p.ev.Start == "continue") && stored != nil && !stored.finished &&
		p.to < stored.position-resumeBehindSlack && !divergedFromSeen {
		return SyncResult{Status: statusNotApplied, Reason: reasonResumeBehind, OtherPlace: mine}, nil
	}
	switch outcome {
	case PlaceStale:
		return SyncResult{Status: statusNotApplied, Reason: reasonOlder}, nil
	case PlaceKeepStored:
		// The stored row stays; the span's place is offered (and kept on the row,
		// unless it is the same device's or about the same place).
		if s.b.DeviceID != stored.deviceID && math.Abs(p.to-stored.position) > otherPlaceMin {
			if err := s.setOther(ctx, key, mine, stored.version); err != nil {
				return SyncResult{}, err
			}
		}
		return SyncResult{Status: statusDiverged, Kept: "stored", OtherPlace: mine}, nil
	case PlaceApplyDiverged:
		lost := &ListeningOtherPlace{Position: stored.position, DeviceID: stored.deviceID,
			DeviceName: stored.deviceName, UpdatedAt: stored.updatedAt}
		if err := s.writePlace(ctx, p, in.Version, false); err != nil {
			return SyncResult{}, err
		}
		if stored.deviceID != s.b.DeviceID && math.Abs(p.to-stored.position) > otherPlaceMin {
			if err := s.setOther(ctx, key, lost, in.Version); err != nil {
				return SyncResult{}, err
			}
		}
		return SyncResult{Status: statusDiverged, Kept: "event", OtherPlace: lost}, nil
	}
	// Applied: an offered place is cleared by a device that had seen it (its base
	// is at or after the version it was recorded at).
	clear := stored != nil && stored.otherVersion.Valid && base != nil && *base >= stored.otherVersion.Int64
	if err := s.writePlace(ctx, p, in.Version, clear); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Status: statusApplied}, nil
}

// writePlace stores a span's place at version: position, length (the span's,
// else the book's, else kept), speed, device, time and version, a start when
// none; never the finish state. clear also drops the offered place.
func (s *syncRun) writePlace(ctx context.Context, p syncPrepared, version int64, clear bool) error {
	key := p.target.Key
	if _, err := s.tx.ExecContext(ctx,
		`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed,
		     version, device_id, device_name, updated_at, started_at)
		 VALUES(?1,?2,?3,?4,?5,0,?6,?7,?8,?9,?10,?11)
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
		     other_version=CASE WHEN ?12 THEN NULL ELSE progress.other_version END`,
		s.b.UserID, key.LibraryID, key.Path, p.to, p.length(), p.speed, version, s.b.DeviceID, s.b.DeviceName,
		formatSessionTime(p.ended), p.started.UTC().Format(time.RFC3339), clear); err != nil {
		return err
	}
	s.written[key] = version
	return nil
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
// second, unless it already was) and moves the place to the end when the
// length is known; unfinish clears the finish and keeps the place. Both bump
// the version and record at as finish_changed_at; the base is ignored. An
// unfinish with no row has nothing to change (applied, nothing written).
func (s *syncRun) finish(ctx context.Context, p syncPrepared) (SyncResult, error) {
	key := p.target.Key
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
			     version, device_id, device_name, updated_at, started_at, finished_at, finish_changed_at)
			 VALUES(?,?,?,?,?,1,1,1,?,?,?,?,?,?)`,
			s.b.UserID, key.LibraryID, key.Path, duration, duration, s.b.DeviceID, s.b.DeviceName, at, day, day,
			at); err != nil {
			return SyncResult{}, err
		}
		s.written[key] = 1
		return SyncResult{Status: statusApplied}, nil
	}
	// The row's time never moves back (a later save elsewhere stays later).
	updated := stored.updatedAt
	if t, err := time.Parse(time.RFC3339, stored.updatedAt); err != nil || p.ended.After(t) {
		updated = at
	}
	version := stored.version + 1
	if p.ev.Kind == EventFinish {
		duration := stored.duration
		if duration <= 0 && p.target.Book != nil {
			duration = p.target.Book.Duration
		}
		_, err = s.tx.ExecContext(ctx,
			`UPDATE progress SET finished = 1, finished_at = CASE WHEN finished THEN finished_at ELSE ?1 END,
			     position = CASE WHEN ?2 > 0 THEN ?2 ELSE position END,
			     duration = CASE WHEN ?2 > 0 THEN ?2 ELSE duration END,
			     version = ?3, device_id = ?4, device_name = ?5, updated_at = ?6, finish_changed_at = ?7
			  WHERE user_id = ?8 AND library_id = ?9 AND rel_path = ?10`,
			day, duration, version, s.b.DeviceID, s.b.DeviceName, updated, at,
			s.b.UserID, key.LibraryID, key.Path)
	} else {
		_, err = s.tx.ExecContext(ctx,
			`UPDATE progress SET finished = 0, finished_at = NULL, version = ?, updated_at = ?,
			     finish_changed_at = ?
			  WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
			version, updated, at, s.b.UserID, key.LibraryID, key.Path)
	}
	if err != nil {
		return SyncResult{}, err
	}
	s.written[key] = version
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
// server's: it extends the token's newest session on the book when the span
// starts within SessionGap of its last save (or later, playback having run on:
// continuousPlayback, within resumeWindow) and not before it began, else opens
// one at the span's start. Its listening is listenedBetween's clip of the span
// (a seek ahead, or a span longer than its wall time, is not listening). Only a
// span ending within livePlaying of now can be marked transcoded.
func (s *syncRun) recordSpan(ctx context.Context, p syncPrepared, listened float64) error {
	hb := Heartbeat{UserID: s.b.UserID, Ref: p.target.Key, TokenID: s.b.TokenID, DeviceName: s.b.DeviceName,
		Client: s.b.Client, Position: p.to, Duration: p.length(), Speed: p.speed,
		Transcoded: p.target.Transcoding && s.now.Sub(p.ended) <= livePlaying}
	open, err := latestSession(ctx, s.tx, hb, p.started.Add(-resumeWindow))
	if err != nil {
		return err
	}
	if open != nil {
		gap := p.started.Sub(parseSessionTime(open.lastAt))
		if p.started.Before(parseSessionTime(open.startedAt)) ||
			(gap > SessionGap && !continuousPlayback(gap, p.from-open.endPos, p.speed)) {
			open = nil
		}
	}
	ended := formatSessionTime(p.ended)
	if open == nil {
		return insertSession(ctx, s.tx, hb, formatSessionTime(p.started), ended, p.from, p.to, listened)
	}
	return extendSession(ctx, s.tx, open.id, hb, ended, p.to, listened)
}

// recordHistory adds a span to the user's listening history: it extends the
// newest row on the book (one recorded here) when that row ended just before
// the span began and where it begins, so a run of 15 s continue spans is one
// row; otherwise it adds a row of its own.
func (s *syncRun) recordHistory(ctx context.Context, p syncPrepared) error {
	key := p.target.Key
	var (
		id      int64
		toPos   float64
		endedAt string
	)
	err := s.tx.QueryRowContext(ctx,
		`SELECT id, to_pos, ended_at FROM listening_history
		  WHERE user_id = ? AND library_id = ? AND rel_path = ? AND import_id = 0
		  ORDER BY ended_at DESC, id DESC LIMIT 1`,
		s.b.UserID, key.LibraryID, key.Path).Scan(&id, &toPos, &endedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	started, ended := formatSessionTime(p.started), formatSessionTime(p.ended)
	if err == nil {
		gap := p.started.Sub(parseSessionTime(endedAt))
		if gap <= historyJoinGap && gap >= -historyJoinOverlap && math.Abs(toPos-p.from) <= historyJoinPos {
			_, err := s.tx.ExecContext(ctx,
				`UPDATE listening_history SET to_pos = ?, ended_at = MAX(ended_at, ?) WHERE id = ?`, p.to, ended, id)
			return err
		}
	}
	_, err = s.tx.ExecContext(ctx,
		`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at)
		 VALUES(?,?,?,?,?,?,?)`, s.b.UserID, key.LibraryID, key.Path, p.from, p.to, started, ended)
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
