package catalog

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
)

// Reading progress (Bridge Phase 3b, migration 0057): a reader's place in a
// book's ebook, path-keyed on the book's own path like progress, with Sync v2's
// conflict rule (ResolvePlace), which listening sync reuses (Track W1,
// SyncListening).

// ReadingProgress is one user's place in a book's ebook: the place as an EPUB
// CFI and as a fraction of the book (Progression, 0-1, what the conflict rule
// compares), the server's count of applied writes (Version), the device that
// wrote it (the client's DeviceID) and that device's name as the server knows it
// (DeviceName, from the token that saved it; "" when unknown).
type ReadingProgress struct {
	Ref
	CFI         string  `json:"cfi"`
	Progression float64 `json:"progression"`
	Version     int64   `json:"version"`
	DeviceID    string  `json:"device_id"`
	DeviceName  string  `json:"device_name"`
	UpdatedAt   string  `json:"updated_at"`
}

// OtherPlace is the place a diverged save did NOT keep (SaveReadingProgress):
// the reader offers it ("On {device} you were at {percent}"). Never stored.
type OtherPlace struct {
	CFI         string  `json:"cfi"`
	Progression float64 `json:"progression"`
	DeviceID    string  `json:"device_id"`
	DeviceName  string  `json:"device_name"`
	UpdatedAt   string  `json:"updated_at"`
}

// ReadingSave is one save of a place (PUT /reading-progress): Ref is the book,
// DeviceName the caller's device as the server knows it (never the body's), and
// BaseVersion the stored version the client last saw (0 = none seen; nil = not
// sent, a save that never diverges).
type ReadingSave struct {
	Ref
	CFI         string
	Progression float64
	DeviceID    string
	DeviceName  string
	UpdatedAt   string
	BaseVersion *int64
}

// Limits of a save's fields (CheckReadingSave).
const (
	maxCFIBytes      = 2048
	maxDeviceIDBytes = 200
)

// CheckReadingSave checks a save's fields before anything is read: a CFI that
// starts "epubcfi(" and is at most 2048 bytes, a progression in [0, 1], a device
// id that is sent (ResolvePlace tells devices apart by it: two saves with none
// would read as one device's, so a save that never saw the other's row would
// move the place back silently) and at most 200 bytes, a base version that isn't
// negative and an updated_at in RFC3339 from year 1 on in UTC (an earlier one's
// stored form, formatSessionTime's, is no RFC3339 time: it would neither parse
// back nor order as text). Its error is the reason (the handler's 400 message).
func CheckReadingSave(in ReadingSave) error {
	switch {
	case !strings.HasPrefix(in.CFI, "epubcfi("):
		return errors.New("cfi must be an EPUB CFI")
	case len(in.CFI) > maxCFIBytes:
		return errors.New("cfi is too long")
	case math.IsNaN(in.Progression) || in.Progression < 0 || in.Progression > 1:
		return errors.New("progression must be from 0 to 1")
	case in.DeviceID == "":
		return errors.New("device_id is required")
	case len(in.DeviceID) > maxDeviceIDBytes:
		return errors.New("device_id is too long")
	case in.BaseVersion != nil && *in.BaseVersion < 0:
		return errors.New("base_version must not be negative")
	}
	if t, err := time.Parse(time.RFC3339, in.UpdatedAt); err != nil || t.UTC().Year() < 1 {
		return errors.New("updated_at must be an RFC3339 time")
	}
	return nil
}

// readingColumns are the columns of a ReadingProgress, in scanReading's order.
const readingColumns = `library_id, rel_path, cfi, progression, version, device_id, device_name, updated_at`

func scanReading(row interface{ Scan(...any) error }, p *ReadingProgress) error {
	return row.Scan(&p.LibraryID, &p.Path, &p.CFI, &p.Progression, &p.Version, &p.DeviceID, &p.DeviceName, &p.UpdatedAt)
}

// GetReadingProgress returns a user's place in the book at ref, or nil if none.
func (c *Catalog) GetReadingProgress(ctx context.Context, userID int64, ref Ref) (*ReadingProgress, error) {
	return getReading(ctx, c.db, userID, ref)
}

// getReading is GetReadingProgress read through q (the reader pool, or a
// transaction).
func getReading(ctx context.Context, q querier, userID int64, ref Ref) (*ReadingProgress, error) {
	var p ReadingProgress
	err := scanReading(q.QueryRowContext(ctx,
		`SELECT `+readingColumns+` FROM reading_progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path), &p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveReadingProgress applies one save (checked: CheckReadingSave) by
// ResolvePlace against the stored row, in one writer transaction, and returns
// the row stored after it and, for a diverged save, the place not kept (nil
// otherwise). An applied save is stored with the version after the stored one
// (1 for the first). Its updated_at is kept as fixed-width UTC milliseconds; a
// far-future one is replaced by server time, as SaveProgress does
// (plausibleUpdatedAt).
func (c *Catalog) SaveReadingProgress(ctx context.Context, userID int64, in ReadingSave) (*ReadingProgress, *OtherPlace, error) {
	// Stored as fixed-width UTC milliseconds (as session times are), so a move's
	// merge (carryListeningState) can compare two rows' times as text.
	if t, err := time.Parse(time.RFC3339, in.UpdatedAt); err == nil && plausibleUpdatedAt(in.UpdatedAt, c.now()) {
		in.UpdatedAt = formatSessionTime(t)
	} else {
		in.UpdatedAt = c.stamp()
	}
	incoming := ReadingProgress{Ref: in.Ref, CFI: in.CFI, Progression: in.Progression, Version: 1,
		DeviceID: in.DeviceID, DeviceName: in.DeviceName, UpdatedAt: in.UpdatedAt}
	var kept *ReadingProgress
	var other *OtherPlace
	err := c.db.WithUserStateTx(ctx, "SaveReadingProgress", func(tx *sql.Tx) error {
		stored, err := getReading(ctx, tx, userID, in.Ref)
		if err != nil {
			return err
		}
		var storedPlace *Place
		if stored != nil {
			incoming.Version = stored.Version + 1
			storedPlace = &Place{Progression: stored.Progression, DeviceID: stored.DeviceID,
				UpdatedAt: stored.UpdatedAt, Version: stored.Version}
		}
		res := ResolvePlace(storedPlace, Place{Progression: incoming.Progression, DeviceID: incoming.DeviceID,
			UpdatedAt: incoming.UpdatedAt, Version: incoming.Version}, in.BaseVersion)
		switch res {
		case PlaceKeepStored:
			kept = stored
			other = otherPlace(incoming)
			return nil
		case PlaceStale:
			kept = stored
			return nil
		case PlaceApplyDiverged:
			other = otherPlace(*stored)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reading_progress(user_id, library_id, rel_path, cfi, progression, version,
			     device_id, device_name, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
			     cfi=excluded.cfi, progression=excluded.progression, version=excluded.version,
			     device_id=excluded.device_id, device_name=excluded.device_name, updated_at=excluded.updated_at`,
			userID, incoming.LibraryID, incoming.Path, incoming.CFI, incoming.Progression, incoming.Version,
			incoming.DeviceID, incoming.DeviceName, incoming.UpdatedAt); err != nil {
			return err
		}
		kept = &incoming
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return kept, other, nil
}

// otherPlace is a row's place as a diverged save's other_place.
func otherPlace(p ReadingProgress) *OtherPlace {
	return &OtherPlace{CFI: p.CFI, Progression: p.Progression, DeviceID: p.DeviceID,
		DeviceName: p.DeviceName, UpdatedAt: p.UpdatedAt}
}

// Place is one device's place in a book as Sync v2's conflict rule sees it (a
// save, or the stored row): how far through the book it is, the device that
// wrote it, when (RFC3339) and the version it has, or for a save the version it
// would be stored with. Progression is any measure that grows through the book:
// a fraction (0-1) for the reader, whole-book seconds for listening
// (SyncListening); ResolvePlace only compares two of one kind with >.
type Place struct {
	Progression float64
	DeviceID    string
	UpdatedAt   string
	Version     int64
}

// PlaceOutcome is ResolvePlace's answer for one save.
type PlaceOutcome int

const (
	// PlaceApply stores the save: there was nothing stored, or it is newer than
	// the stored row (last-write-wins, IsNewer's order).
	PlaceApply PlaceOutcome = iota
	// PlaceStale keeps the stored row: an older save that didn't diverge. It is
	// echoed back with no other place.
	PlaceStale
	// PlaceApplyDiverged stores the save, which diverged and is further on; the
	// stored row is the other place.
	PlaceApplyDiverged
	// PlaceKeepStored keeps the stored row against a save that diverged and isn't
	// further on; the save is the other place.
	PlaceKeepStored
)

// ResolvePlace is Sync v2's conflict rule for one save (in) against the stored
// row (nil for none), given the stored version the saving device last saw
// (base: 0 = none seen; nil = not sent). The save diverged, made without seeing
// the stored row, when a row is stored and base is sent and is either 0 (whatever
// the device: a device id outlives the places a client kept, so signed out and
// back in its start would otherwise replace its own place further on) or not the
// row's version while another device wrote the row (one device's saves in flight
// together share a base). So a device's first saves on a book (all on base 0)
// diverge from each other when one lands before the next is answered, keeping
// the further place: the safe side. A diverged save never moves the place back
// silently: the further place by progression is kept (the save when it is
// further, else the stored row; a tie keeps the stored row) and the other is
// offered. Any other save (the same device's own saves on
// a version it saw, one that saw the stored row, one sent with no base) is
// last-write-wins exactly like progress (IsNewer: the later updated_at, the
// version breaking a tie), so moving back on purpose works. Pure: reading
// progress and listening sync (SyncListening, Track W1) use it.
func ResolvePlace(stored *Place, in Place, base *int64) PlaceOutcome {
	switch {
	case stored == nil:
		return PlaceApply
	case base != nil && (*base == 0 || (*base != stored.Version && stored.DeviceID != in.DeviceID)):
		if in.Progression > stored.Progression {
			return PlaceApplyDiverged
		}
		return PlaceKeepStored
	case newerStamp(in.UpdatedAt, in.Version, stored.UpdatedAt, stored.Version):
		return PlaceApply
	default:
		return PlaceStale
	}
}
