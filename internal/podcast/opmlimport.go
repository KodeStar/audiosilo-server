package podcast

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
)

// ImportNewRequests is how many new podcast.subscribe jobs one OPML upload may
// create, whatever else is open: the rest of the rows answer limited, for the
// next upload (a wave). Adopting an open request or following a known show
// spends none of it; the five-open-per-person cap applies besides.
const ImportNewRequests = 5

// The OPML import's row statuses.
const (
	RowFollowed         = "followed"
	RowAlreadyFollowing = "already_following"
	RowQueued           = "queued"
	RowAlreadyQueued    = "already_queued"
	RowDuplicate        = "duplicate"
	RowInvalid          = "invalid"
	RowLimited          = "limited"
	RowUnavailable      = "unavailable"
	RowFailed           = "error"
)

// RowError is a row's safe error: never the document's text or a raw URL.
type RowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImportRow is one subscription candidate's decision.
type ImportRow struct {
	OutlineIndex int       `json:"outline_index"`
	Status       string    `json:"status"`
	Feed         *string   `json:"feed"`
	Private      bool      `json:"private"`
	Path         *string   `json:"path"`
	RequestID    *int64    `json:"request_id"`
	DuplicateOf  *int      `json:"duplicate_of"`
	Error        *RowError `json:"error"`
}

// ImportResult is POST /podcasts/opml/import's answer: decisions, not
// completion (a queued row's request is polled on /me/podcasts).
type ImportResult struct {
	LibraryID int64       `json:"library_id"`
	Rows      []ImportRow `json:"rows"`
}

// Follow errors' codes and messages, shared by a single follow's envelope and
// an import's rows.
var followErrors = map[string]RowError{
	"missing_feed_url":     {"missing_feed_url", "A feed URL is required."},
	"invalid_feed_url":     {"invalid_feed_url", "Invalid feed URL."},
	"too_many_requests":    {"too_many_requests", "At most five follow requests may be open."},
	"bridge_unavailable":   {"bridge_unavailable", "No online Bridge can perform this podcast action."},
	"library_not_writable": {"library_not_writable", "The Bridge cannot write to this library."},
	"module_off":           {"module_off", "this module is turned off"},
	"privacy_conflict":     {"privacy_conflict", "An open follow request for this feed has the other privacy."},
	"no_library_access":    {"no_library_access", "Following a URL needs access to the whole library."},
	"subscribe_failed":     {"subscribe_failed", "Could not follow this feed."},
}

// FollowError is a follow refusal's code and message ("" for an unexpected
// error, which the caller logs and sanitizes).
func FollowError(err error) (RowError, string) {
	var code, status string
	switch {
	case errors.Is(err, feed.ErrInvalidFeedURL):
		code, status = "invalid_feed_url", RowInvalid
	case errors.Is(err, bridge.ErrTooManyRequests), errors.Is(err, bridge.ErrNoNewRequest):
		code, status = "too_many_requests", RowLimited
	case errors.Is(err, ErrBridgeUnavailable), errors.Is(err, bridge.ErrKindUnavailable):
		code, status = "bridge_unavailable", RowUnavailable
	case errors.Is(err, bridge.ErrLibraryNotWritable):
		code, status = "library_not_writable", RowUnavailable
	case errors.Is(err, ErrModuleOff):
		code, status = "module_off", RowUnavailable
	case errors.Is(err, bridge.ErrPrivacyConflict):
		code, status = "privacy_conflict", RowFailed
	case errors.Is(err, ErrNoWholeGrant):
		code, status = "no_library_access", RowFailed
	default:
		return followErrors["subscribe_failed"], ""
	}
	return followErrors[code], status
}

// Import acts on a validated document's candidates in document order, each
// as the caller in lib (a podcast library the caller holds whole, checked
// before the body was read; the grant and the module are checked again per
// row). Within the upload the first outline with a normalized URL wins, later
// ones are duplicates of it (even when it failed). Each row commits on its own:
// a later failure never undoes an earlier row, and re-uploading is the retry.
// ctx ending stops the rows not yet acted on (they are not reported).
func (f *Follower) Import(ctx context.Context, caller catalog.Caller, lib int64, doc *opml.Document) ImportResult {
	out := ImportResult{LibraryID: lib, Rows: []ImportRow{}}
	first := map[string]int{}
	created := 0
	for _, c := range doc.Candidates {
		if ctx.Err() != nil {
			break
		}
		row := ImportRow{OutlineIndex: c.Index, Private: true}
		if c.URL == "" {
			row.fail(RowInvalid, followErrors["missing_feed_url"])
			out.Rows = append(out.Rows, row)
			continue
		}
		norm, err := feed.NormalizeStable(c.URL)
		if err != nil {
			row.fail(RowInvalid, followErrors["invalid_feed_url"])
			out.Rows = append(out.Rows, row)
			continue
		}
		label := feed.RedactURL(norm)
		row.Feed, row.Private = &label, feed.IsPrivateFeedURL(norm)
		if idx, dup := first[norm]; dup {
			row.Status, row.DuplicateOf = RowDuplicate, &idx
			out.Rows = append(out.Rows, row)
			continue
		}
		first[norm] = c.Index
		res, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: lib, FeedURL: norm, AdoptOnly: created >= ImportNewRequests})
		if err != nil {
			e, status := FollowError(err)
			if status == "" {
				status = RowFailed
				if f.Log != nil {
					f.Log.Warn("opml import row failed", "library", lib, "outline", c.Index, "err", err)
				}
			}
			row.fail(status, e)
			out.Rows = append(out.Rows, row)
			continue
		}
		row.Private = res.Private
		switch res.State {
		case StateFollowing, StateAlreadyFollowing:
			row.Status = RowFollowed
			if res.State == StateAlreadyFollowing {
				row.Status = RowAlreadyFollowing
			}
			p := res.Show.Path
			row.Path = &p
		case StateSubscribing:
			row.Status = RowAlreadyQueued
			if res.Created {
				row.Status = RowQueued
				created++
			}
			id := res.Job.ID
			row.RequestID = &id
			if p := subscribeShowPath(res.Job); p != "" {
				row.Path = &p
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

func (r *ImportRow) fail(status string, e RowError) {
	r.Status, r.Path, r.RequestID, r.DuplicateOf = status, nil, nil, nil
	r.Error = &e
}

// subscribeShowPath is the show a subscribe repairs ("" for a new show).
func subscribeShowPath(j *bridge.Job) string {
	var p bridge.SubscribeParams
	_ = json.Unmarshal(j.Params, &p)
	return p.ShowPath
}
