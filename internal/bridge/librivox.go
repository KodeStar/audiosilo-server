package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// LibriVox L6: adding a LibriVox recording to a books library. The admin picks
// a recording from the community metadata database (the server resolves its
// identity: meta.Service.LibrivoxRecordingOf); the Bridge reads the project's
// manifest from its own metadata service, downloads its sections from
// archive.org, places them as a new book folder with its cover and a
// .audiosilo/librivox.json, rescans it, and reports the book's path; the server
// then records the book's community match (catalog.FoldLibrivoxFetch).

const (
	// KindLibrivoxFetch downloads one LibriVox project into a books library.
	// Params LibrivoxFetchParams; the server queues it (internal), one open per
	// library and project (librivoxFetchSuffix). Not a module's: an admin, the
	// metadata service on and a Bridge that runs it are all it needs.
	KindLibrivoxFetch = "librivox.fetch"
	// FeatureLibrivox is the feature of a Bridge that runs KindLibrivoxFetch
	// (its family): the console offers "Add from LibriVox" only with it.
	FeatureLibrivox = "librivox"
)

// LibrivoxFetchParams are librivox.fetch's params, built by the server when it
// queues the job: the LibriVox project, the community work and recording that
// project IS (from the metadata service's lookup at queue time: what the done
// job's book is recorded as), and the books library (the job's library_id
// too). The Bridge reads them leniently.
type LibrivoxFetchParams struct {
	LibrivoxID  int    `json:"librivox_id"`
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	LibraryID   int64  `json:"library_id"`
}

// librivoxFetchParams checks librivox.fetch's params strictly: a positive
// project id, a work and a recording (bounded, no control characters, never
// blank: the identity the fold records) and a library.
func librivoxFetchParams(raw json.RawMessage) (json.RawMessage, error) {
	var p LibrivoxFetchParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	switch {
	case p.LibrivoxID <= 0:
		return nil, invalid("librivox_id must be a LibriVox project id")
	case strings.TrimSpace(p.WorkID) == "":
		return nil, invalid("work_id is required")
	case strings.TrimSpace(p.RecordingID) == "":
		return nil, invalid("recording_id is required")
	case p.LibraryID <= 0:
		return nil, invalid("library_id is required")
	}
	if err := checkText("work_id", p.WorkID, maxWorkIDLen); err != nil {
		return nil, err
	}
	if err := checkText("recording_id", p.RecordingID, maxWorkIDLen); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// librivoxFetchSuffix extends a librivox.fetch's dedupe key with its project,
// so a library has at most one open fetch of each project.
func librivoxFetchSuffix(j NewJob) string {
	var p LibrivoxFetchParams
	_ = json.Unmarshal(j.Params, &p)
	return "|" + strconv.Itoa(p.LibrivoxID)
}

// LibrivoxFetch is the NewJob that adds p's project to p's library, asked for
// by admin requestedBy.
func LibrivoxFetch(p LibrivoxFetchParams, requestedBy int64) NewJob {
	raw, _ := json.Marshal(p)
	lib := p.LibraryID
	return NewJob{Kind: KindLibrivoxFetch, LibraryID: &lib, Params: raw, RequestedBy: requestedBy}
}

// LibrivoxFetchResult is a done librivox.fetch's result: the placed book's
// indexed path (as the Bridge's rescan answered it), the project, and how many
// section files and bytes were written.
type LibrivoxFetchResult struct {
	LibraryID  int64  `json:"library_id"`
	Path       string `json:"path"`
	LibrivoxID int    `json:"librivox_id"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
}

// LibrivoxFetchOf reads a job's params and, when it is done, its result.
func LibrivoxFetchOf(j Job) (p LibrivoxFetchParams, r LibrivoxFetchResult, err error) {
	if err = json.Unmarshal(j.Params, &p); err != nil {
		return p, r, err
	}
	if j.State == StateDone && len(j.Result) > 0 {
		err = json.Unmarshal(j.Result, &r)
	}
	return p, r, err
}

// OpenLibrivoxFetches are the open librivox.fetch jobs of the projects ids,
// in any library, by project: the newest one of each.
func (s *Service) OpenLibrivoxFetches(ctx context.Context, ids []int) (map[int]*Job, error) {
	out := map[int]*Job{}
	if len(ids) == 0 {
		return out, nil
	}
	raw, _ := json.Marshal(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind = ? AND j.finished_at IS NULL
		AND j.state IN `+OpenStatesSQL+`
		AND json_extract(j.params, '$.librivox_id') IN (SELECT value FROM json_each(?)) ORDER BY j.id`,
		KindLibrivoxFetch, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		var p LibrivoxFetchParams
		if json.Unmarshal(j.Params, &p) == nil && p.LibrivoxID > 0 {
			out[p.LibrivoxID] = j // ordered by id: the newest wins
		}
	}
	return out, rows.Err()
}
