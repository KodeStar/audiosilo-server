package bridge

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/kodestar/audiosilo-meta/pkg/model"
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
// job's book is recorded as), the books library (the job's library_id too),
// and the work's title then (additive, server#380: a fetch that never placed
// a book is named by it in a notification; the Bridge ignores it). The Bridge
// reads them leniently.
type LibrivoxFetchParams struct {
	LibrivoxID  int    `json:"librivox_id"`
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	LibraryID   int64  `json:"library_id"`
	Title       string `json:"title,omitempty"`
}

// maxLibrivoxTitle bounds a librivox.fetch's title in bytes (the work's, from
// the metadata service; LibrivoxFetch shortens a longer one).
const maxLibrivoxTitle = 300

// librivoxFetchParams checks librivox.fetch's params strictly: a positive
// project id, a work and a recording (community ids, model.ValidSlug as
// contribute and upcoming check them: the identity the fold records; checked
// at queue time only, so a job queued before is never refused later), a library and an optional title (bounded, no
// control characters).
func librivoxFetchParams(raw json.RawMessage) (json.RawMessage, error) {
	var p LibrivoxFetchParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	switch {
	case p.LibrivoxID <= 0:
		return nil, invalid("librivox_id must be a LibriVox project id")
	case !model.ValidSlug(p.WorkID):
		return nil, invalid("work_id must be a community work id")
	case !model.ValidSlug(p.RecordingID):
		return nil, invalid("recording_id must be a community recording id")
	case p.LibraryID <= 0:
		return nil, invalid("library_id is required")
	}
	if err := checkText("title", p.Title, maxLibrivoxTitle); err != nil {
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
// by admin requestedBy. A title is kept to one line of at most
// maxLibrivoxTitle bytes.
func LibrivoxFetch(p LibrivoxFetchParams, requestedBy int64) NewJob {
	p.Title = CleanText(p.Title, maxLibrivoxTitle)
	raw, _ := json.Marshal(p)
	lib := p.LibraryID
	return NewJob{Kind: KindLibrivoxFetch, LibraryID: &lib, Params: raw, RequestedBy: requestedBy}
}

// LibrivoxFetchResult is a done librivox.fetch's result: the placed book's
// indexed path (as the Bridge's rescan answered it), the project, and how many
// files and bytes the book's folder holds (the sections and the cover).
// Adopted (additive, server#379) says whether the Bridge adopted a folder it
// had placed before (true: an earlier fetch's book, which an admin may have
// matched or edited since) or placed a new one in a vacant folder (false);
// nil from a Bridge that doesn't say (catalog.FoldLibrivoxFetch keeps its
// older rule then).
type LibrivoxFetchResult struct {
	LibraryID  int64  `json:"library_id"`
	Path       string `json:"path"`
	LibrivoxID int    `json:"librivox_id"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
	Adopted    *bool  `json:"adopted,omitempty"`
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
// in any library, by project: the newest one of each. A lease that ran out is
// settled first, as every read of the open jobs does (expireDue), so a fetch
// that failed on its last attempt is not listed as open.
func (s *Service) OpenLibrivoxFetches(ctx context.Context, ids []int) (map[int]*Job, error) {
	out := map[int]*Job{}
	if len(ids) == 0 {
		return out, nil
	}
	if err := s.expireDue(ctx); err != nil {
		return nil, err
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
