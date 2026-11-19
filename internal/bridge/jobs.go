package bridge

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// Job states (migration 0047).
const (
	StateQueued         = "queued"
	StateClaimed        = "claimed"
	StateRunning        = "running"
	StateDone           = "done"
	StateFailed         = "failed"
	StateNeedsAttention = "needs_attention"
	StateCancelled      = "cancelled"
)

// OpenStatesSQL are the states a job is still the Bridge's business in, as an
// SQL list: a kind, library and path have at most one job in them (dedupe_key;
// migration 0047's partial index names the same four).
const OpenStatesSQL = `('queued', 'claimed', 'running', 'needs_attention')`

// sqlLiteral is SQL text written in the source. A function that takes one
// (ParamSQL) can only be handed string constants: a string read at run time
// doesn't convert to it without naming the type, which no other package can
// and this one never does, so nothing a request or a row carries can reach
// the statement it is written into.
type sqlLiteral string

// ParamSQL is SQL reading path ('$.account') from col, a bridge_jobs JSON
// column (params, result), as NULL when what it stores is not JSON (a damaged row):
// json_extract on one fails the whole statement, and SQLite does not
// short-circuit a json_valid beside it in an AND. Both are written into the
// statement as they are, so they are constants by their type (sqlLiteral).
func ParamSQL(col, path sqlLiteral) string {
	return "CASE WHEN json_valid(" + string(col) + ") THEN json_extract(" + string(col) + ",'" + string(path) + "') END"
}

// IsOpenState reports whether state is one of OpenStatesSQL's.
func IsOpenState(state string) bool {
	switch state {
	case StateQueued, StateClaimed, StateRunning, StateNeedsAttention:
		return true
	}
	return false
}

const (
	// Lease is how long a claim holds a job without a progress update. The
	// Bridge reports at least every 30 s while it works.
	Lease = 2 * time.Minute
	// MaxAttempts is how many times a job may be claimed; a lease that runs out
	// on the last attempt fails the job instead of queueing it again.
	MaxAttempts = 3
	// MaxWait bounds a claim's long poll, under the server's 30 s request
	// timeout.
	MaxWait = 25 * time.Second

	// MaxListLimit and DefaultListLimit bound List's page.
	MaxListLimit     = 200
	DefaultListLimit = 50

	// defaultParamsBytes bounds a job's params as sent, unless its kind sets
	// its own (kindSpec.maxParams); MaxParamsBytes is the most any kind an
	// admin may queue takes (the console's POST /admin/bridge/jobs body; an
	// internal kind's params are the server's own).
	defaultParamsBytes = 16 << 10
	MaxParamsBytes     = max(defaultParamsBytes, maxCorrectParamsBytes)
	maxProgressBytes   = 16 << 10
	maxResultBytes     = 64 << 10
	maxErrorBytes      = 2 << 10

	// releaseTimeout bounds settleUnsentClaim.
	releaseTimeout = 5 * time.Second
)

// KindVerify is Phase 0's proof job: the Bridge stats a book (or a library's
// root) through its mapping and reports what it found.
const KindVerify = "bridge.verify"

// KindSidecars is Phase 1a's job: the Bridge generates a book's characters and
// recaps (and, since Phase 1c, its description) and writes them into the book's
// .audiosilo/ folder.
const KindSidecars = "sidecars.generate"

// KindDescribe is Phase 1c's description-only job: the Bridge writes a book's
// own-words description from the notes it kept from generating its characters
// and recaps (no new speech recognition), into the same .audiosilo/ folder.
const KindDescribe = "sidecars.describe"

// KindDetectChapters is Phase 1d's chapter detection: for a book with no
// chapter markers, the Bridge transcribes the audio and finds the chapters in
// the narration (no agent), and writes them into the book's .audiosilo/ folder
// as chapters.json, which the server then uses as the book's chapters.
const KindDetectChapters = "chapters.detect"

// KindContribute is Phase 2b's "Share with the community": the Bridge files a
// book's characters and recaps, generated earlier on this server and kept on
// the Bridge, to the community metadata database as intake issues (what a
// sidecars.generate run with contribute "issue" files at its end). It writes
// nothing into the library.
const KindContribute = "sidecars.contribute"

// KindCorrect is Phase 2b's "Suggest corrections": the Bridge files an admin's
// edits of a matched book's details (CorrectParams) to the community metadata
// database as one corrections issue. It writes nothing into the library. Its
// family is the meta feature.
const KindCorrect = "meta.correct"

// KindReadalong is Phase 3c's read-along: the Bridge aligns a book's ebook
// with its narration (reusing the transcript it kept, else transcribing; no
// agent) and writes the map into the book's .audiosilo/ folder as
// readalong.json. Its family is the readalong feature.
const KindReadalong = "readalong.align"

// ModuleContributions is the managed module (its id: internal/modules/builtin)
// a job that files to the community needs (kindSpec.module, ModuleFor): its
// routes and jobs answer 404 module_off while it is off.
const ModuleContributions = "contributions"

// bookRule is whether a kind's job names a book.
type bookRule int

const (
	bookNone     bookRule = iota // never a path
	bookOptional                 // a book, or the library itself
	bookRequired                 // always a book
	bookShow
	bookEpisode
)

// kindSpec is what the server knows about a job kind: whether a job of it is
// about a library and a book, whether it writes into the library, how to check
// its params, and how it stands with the other kinds' open jobs for the book.
type kindSpec struct {
	// internal: only the server queues it (never the admin's POST /admin/bridge/jobs).
	internal bool
	// quiet: never notified (Job.Notable).
	quiet bool
	// libraryKind is the kind of library a job of it is for ("": any).
	libraryKind string
	// suffix extends the dedupe key (a subscribe's feed hash and requester).
	suffix       func(NewJob) string
	needsLibrary bool
	book         bookRule
	// writes: the job writes into the library, so it is queued only for a
	// library the Bridge's last heartbeat reports mapped and writable.
	writes bool
	params func(json.RawMessage) (json.RawMessage, error)
	// refusedWhileOpen: a new job is refused with the error while a job of the
	// kind named is open for the same book (Service.blocker): never by one being
	// cancelled (cancel_requested: it ends without writing), and, when the new
	// job also cancels that kind (cancelsOpen), only while it is queued or
	// working: a parked one gives way, and so does a queued one held while
	// its module is off (Service.givesWay, Service.held).
	refusedWhileOpen map[string]error
	// cancelsOpen: a new job cancels the open job of each kind named for the
	// same book (cancelOpenTx, the value its error), in the same transaction.
	cancelsOpen map[string]string
	// module is the managed module a job of the kind needs ("" for none): the
	// api answers 404 module_off while it is off (ModuleFor), and a queued one
	// is held meanwhile (Service.held; a generate job that shares along is not
	// held from a claim that honours ShareOff). A job that needs ModuleContributions
	// files to the community, so it also needs a Bridge that can
	// (ErrCannotContribute), and one of a kind that only files (the kind's own
	// module is ModuleContributions) is asked to stop at its next report once
	// the module is off (Service.stopsShare); a sidecars.generate job that
	// shares along is told to keep its results local instead (Job.ShareOff).
	module string
	// maxParams bounds the params as sent, in bytes (0: defaultParamsBytes).
	maxParams int
	// promotes: a request that finds a job of the kind already open raises
	// the open job's priority to its own, never lowers it (Check now on a
	// scheduled audible.poll; a podcast fetch does the same through its
	// demand, NewJob.fetch), and gives a job still queued its params (an
	// audible.poll's template, changed since the scheduler queued it).
	promotes bool
}

// paramsCap is the most bytes of params a job of the kind may send.
func (s kindSpec) paramsCap() int { return cmp.Or(s.maxParams, defaultParamsBytes) }

// kinds are the job kinds the server can queue (and check). A kind the Bridge
// advertises that isn't here is never queued, and neither is one here that the
// Bridge doesn't advertise. A describe or detect job waits for an open generate
// job (ErrGenerateOpen); a generate job waits for a queued or working detect job
// (ErrDetectOpen) and cancels a parked one, and cancels an open describe job;
// describe and detect, which both rewrite the book's bridge.json on the Bridge,
// wait for each other (ErrDetectOpen, ErrDescribeOpen). A contribute job,
// which files what the last generate run made, waits for an open generate job
// (ErrGenerateOpen); a generate job waits for a queued or working contribute job
// (ErrContributeOpen: it would replace what is being filed) and cancels a parked
// one, or a queued one held while the contributions module is off. A
// read-along job and a generate or detect job, which all transcribe the same
// audio on the Bridge (one engine book per audio source), never overlap: a
// read-along job waits for an open generate or detect job (ErrGenerateOpen,
// ErrDetectOpen), a detect job for an open read-along job (ErrReadalongOpen),
// and a generate job for a queued or working one, cancelling a parked one.
var kinds = map[string]kindSpec{
	KindPodcastFetch:     {needsLibrary: true, book: bookEpisode, writes: true, params: fetchParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindPodcastRetain:    {needsLibrary: true, book: bookShow, writes: true, params: retainParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindPodcastSubscribe: {needsLibrary: true, book: bookNone, writes: true, params: subscribeParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true, suffix: subscribeSuffix},
	KindPodcastPoll:      {needsLibrary: true, book: bookShow, writes: true, params: pollParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindVerify:           {needsLibrary: true, book: bookOptional, params: verifyParams},
	KindAudiblePoll:      {book: bookNone, params: audiblePollParams, module: ModuleAudible, internal: true, quiet: true, suffix: audibleAccountSuffix, promotes: true},
	KindAudibleSync:      {book: bookNone, params: audibleSyncParams, module: ModuleAudible, internal: true, quiet: true, suffix: audibleAccountSuffix, promotes: true},
	KindAudibleImport: {needsLibrary: true, book: bookNone, writes: true, libraryKind: libraryBooks, params: audibleImportParams,
		module: ModuleAudible, internal: true, suffix: audibleImportSuffix},
	KindLibrivoxFetch: {needsLibrary: true, book: bookNone, writes: true, libraryKind: libraryBooks, params: librivoxFetchParams,
		internal: true, suffix: librivoxFetchSuffix},
	KindSidecars: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: sidecarsParams,
		refusedWhileOpen: map[string]error{KindDetectChapters: ErrDetectOpen, KindContribute: ErrContributeOpen,
			KindReadalong: ErrReadalongOpen},
		cancelsOpen: map[string]string{
			KindDescribe:       "a generate job for the book replaced it",
			KindDetectChapters: "a generate job for the book replaced it; it finds the chapters itself",
			KindContribute:     "a generate job for the book replaced what it was sharing",
			KindReadalong:      "a generate job for the book replaced it; make the read-along again after",
		}},
	KindContribute: {libraryKind: libraryBooks, needsLibrary: true, book: bookRequired, params: contributeParams, module: ModuleContributions,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen}},
	KindCorrect: {libraryKind: libraryBooks, needsLibrary: true, book: bookRequired, params: correctParams, module: ModuleContributions,
		maxParams: maxCorrectParamsBytes},
	KindDescribe: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: noParams,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen, KindDetectChapters: ErrDetectOpen}},
	KindDetectChapters: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: noParams,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen, KindDescribe: ErrDescribeOpen,
			KindReadalong: ErrReadalongOpen}},
	KindReadalong: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: noParams,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen, KindDetectChapters: ErrDetectOpen}},
}

// claimedStatesSQL are the open states of a job a claim handed to the Bridge
// that it hasn't parked, the ones holding a lease (lapsedSQL: migration 0047's
// idx_bridge_jobs_lease names the same two, in the same order, or expiry
// scans the table).
const claimedStatesSQL = `('claimed', 'running')`

// workingStatesSQL is OpenStatesSQL less needs_attention: the open jobs not
// parked.
const workingStatesSQL = `('queued', 'claimed', 'running')`

// moduleOff reports whether the managed module id is off (with no ModuleOn
// wired every module counts as on).
func (s *Service) moduleOff(id string) bool {
	return s.ModuleOn != nil && !s.ModuleOn(id)
}

// held reports whether a queued job of kind with params waits for a managed
// module that is off (ModuleFor, moduleOff): no claim hands it out
// (nextQueued; but a claim that honours ShareOff is handed one held only for
// its share, sharesAlong), and it refuses no new job that cancels it (givesWay).
func (s *Service) held(kind string, params json.RawMessage) bool {
	m := ModuleFor(kind, params)
	return m != "" && s.moduleOff(m)
}

// stopsShare reports whether a job of kind the Bridge works on is asked to
// stop (UpdateHeld, CancelModuleOff): one of a kind that only files to the
// community (sidecars.contribute, meta.correct: the kind's own module is
// ModuleContributions) while the Contributions module is off. Such a job
// writes nothing, so its stop loses no work. Every other job a claim handed
// out carries on with its module off: a podcast or Audible job, and a
// sidecars.generate job that contributes, whose stop would throw away the
// characters and recaps it is writing: it is told to keep them local instead
// (Job.ShareOff).
func (s *Service) stopsShare(kind string) bool {
	return kinds[kind].module == ModuleContributions && s.moduleOff(ModuleContributions)
}

// sharesAlong reports whether a job of kind with params files to the community
// only as a step of its own work: a sidecars.generate job with contribute
// "issue", which ModuleFor gives ModuleContributions though its kind's own
// module is none.
func sharesAlong(kind string, params json.RawMessage) bool {
	return kind == KindSidecars && contributes(params)
}

// flagShareOff sets j.ShareOff for the Bridge's answer: a job that shares
// along (sharesAlong) while the Contributions module is off keeps its results
// local, its work carried on (server#218).
func (s *Service) flagShareOff(j *Job) {
	j.ShareOff = s.moduleOff(ModuleContributions) && sharesAlong(j.Kind, j.Params)
}

// blocker is the open job that refuses a new job of kind for the book
// (refusedWhileOpen, its kinds asked in name order so the answer is stable;
// the refusal is refusedWhileOpen[job.Kind]), nil when none does.
func (s *Service) blocker(ctx context.Context, q rowQuerier, kind string, libraryID *int64, path *string) (*Job, error) {
	spec := kinds[kind]
	for _, k := range slices.Sorted(maps.Keys(spec.refusedWhileOpen)) {
		j, err := openJob(ctx, q, DedupeKey(k, libraryID, path))
		if err != nil {
			return nil, err
		}
		if j != nil && !s.givesWay(spec, j) {
			return j, nil
		}
	}
	return nil, nil
}

// givesWay reports whether open job j, of a kind spec's refusedWhileOpen
// names, lets a new job of spec's kind be queued: while it is being cancelled
// (cancel_requested: it ends without writing), and, when the new job cancels
// it (cancelsOpen), while it is parked, or queued but held (no claim hands it
// out while its module is off).
func (s *Service) givesWay(spec kindSpec, j *Job) bool {
	if j.CancelRequested {
		return true
	}
	if _, cancels := spec.cancelsOpen[j.Kind]; !cancels {
		return false
	}
	return j.State == StateNeedsAttention || (j.State == StateQueued && s.held(j.Kind, j.Params))
}

// openJob is the open job with dedupe key key, read through q, nil when there
// is none. There is at most one: the query names migration 0047's partial
// unique index's states (OpenStatesSQL), so it seeks that index.
func openJob(ctx context.Context, q rowQuerier, key string) (*Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+
		` WHERE j.dedupe_key = ? AND j.state IN `+OpenStatesSQL, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// sidecars.generate's param values (auto and local are the defaults),
// and the longest work id it takes.
const (
	SourceAuto      = "auto"
	SourceAudio     = "audio"
	SourceEbook     = "ebook"
	ContributeLocal = "local"
	ContributeIssue = "issue"
	maxWorkIDLen    = 200
)

// SidecarsParams are sidecars.generate's params as stored: what the run reads
// (auto picks an ebook when one sits beside the audio), whether the results
// stay on this server or are also offered upstream as an intake issue, and the
// community work they are for when the caller knows it.
type SidecarsParams struct {
	Source     string `json:"source"`
	Contribute string `json:"contribute"`
	WorkID     string `json:"work_id,omitempty"`
}

// sidecarsParams checks sidecars.generate's params strictly (an unknown field
// is refused) and returns them with the defaults filled in, so the Bridge reads
// one complete object.
func sidecarsParams(raw json.RawMessage) (json.RawMessage, error) {
	var p SidecarsParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if p.Source == "" {
		p.Source = SourceAuto
	}
	if p.Contribute == "" {
		p.Contribute = ContributeLocal
	}
	if !slices.Contains([]string{SourceAuto, SourceAudio, SourceEbook}, p.Source) {
		return nil, invalid("source must be auto, audio or ebook")
	}
	if !slices.Contains([]string{ContributeLocal, ContributeIssue}, p.Contribute) {
		return nil, invalid("contribute must be local or issue")
	}
	// The work id is the caller's: the same hygiene as GET /meta/work's id (at
	// most 200 bytes, no control characters), not a slug grammar.
	if err := checkText("work_id", p.WorkID, maxWorkIDLen); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// contributes reports whether sidecars.generate's params ask for the results
// to be offered to the community (read loosely: checkNew checks them).
func contributes(params json.RawMessage) bool {
	var p SidecarsParams
	return json.Unmarshal(params, &p) == nil && p.Contribute == ContributeIssue
}

// ModuleFor is the managed module a job of kind with params needs ("" for
// none, and for a kind the server doesn't know): the kind's own
// (kindSpec.module), and for sidecars.generate, ModuleContributions when it
// asks to contribute ("issue"). The api answers 404 module_off for such a job
// while the module is off, before anything else about it is checked, and no
// claim hands out a queued one meanwhile (nextQueued), but for a generate job
// that shares along to a Bridge that honours ShareOff (ClaimOptions.ShareOff).
func ModuleFor(kind string, params json.RawMessage) string {
	if sharesAlong(kind, params) {
		return ModuleContributions
	}
	return kinds[kind].module
}

// noParams checks the params of a kind that takes none (sidecars.describe,
// chapters.detect, readalong.align): only an empty object (or none) passes, any field is
// refused, and it is stored as {}.
func noParams(raw json.RawMessage) (json.RawMessage, error) {
	var p struct{}
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	return json.RawMessage(`{}`), nil
}

// verifyParams checks bridge.verify's params: an optional hold_seconds (0-600)
// the Bridge waits before finishing, so a test can cancel it or kill the Bridge
// mid-job.
func verifyParams(raw json.RawMessage) (json.RawMessage, error) {
	var p struct {
		HoldSeconds int `json:"hold_seconds,omitempty"`
	}
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if p.HoldSeconds < 0 || p.HoldSeconds > 600 {
		return nil, invalid("hold_seconds must be 0-600")
	}
	return json.Marshal(p)
}

// decodeStrict decodes one JSON object into v, refusing unknown fields and
// trailing data. Empty input is an empty object.
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// Known reports whether the server knows kind.
func Known(kind string) bool {
	_, ok := kinds[kind]
	return ok
}

// Job is one bridge_jobs row on the wire (admin and Bridge routes alike).
type Job struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	LibraryID *int64          `json:"library_id"`
	Path      *string         `json:"path"`
	Params    json.RawMessage `json:"params"`
	State     string          `json:"state"`
	Progress  json.RawMessage `json:"progress"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
	// RequestedBy is the listener or admin who asked, nil once they are deleted.
	RequestedBy     *Requester `json:"requested_by"`
	FeedRedacted    string     `json:"-"`
	Priority        int        `json:"priority"`
	Attempts        int        `json:"attempts"`
	LeaseUntil      *string    `json:"lease_until"`
	CancelRequested bool       `json:"cancel_requested"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	ClaimedAt       *string    `json:"claimed_at"`
	FinishedAt      *string    `json:"finished_at"`
	// ParamsUnreadable marks a job whose stored params are not JSON (a damaged
	// row), read as params null (scanJob, readableJob), so one such row never
	// blanks a response (a whole job list); a claim ends such a job
	// (claimPriority).
	ParamsUnreadable bool `json:"params_unreadable,omitempty"`
	// ShareOff tells the Bridge to keep a job's results local, filing nothing
	// to the community: a sidecars.generate job with contribute "issue" while
	// the Contributions module is off (Service.flagShareOff). Set only on the
	// Bridge's claim and report answers, never stored: the Bridge reads it at
	// each answer and, once heard, never shares for the job (server#218).
	// Additive: omitted otherwise, and an older Bridge ignores it.
	ShareOff bool `json:"share_off,omitempty"`
}

// Target is the job's book path for an audit entry ("" for a job about a whole
// library or none).
func (j *Job) Target() string {
	if j.Path == nil {
		return ""
	}
	return *j.Path
}

// Requester names the listener or admin who queued a job.
type Requester struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

const jobColumns = `j.id, j.kind, j.library_id, j.rel_path, j.params, j.state, j.progress, j.result, j.error,
	j.requested_by, u.username, j.priority, j.attempts, j.lease_until, j.cancel_requested,
	j.created_at, j.updated_at, j.claimed_at, j.finished_at, j.feed_redacted`

const jobFrom = ` FROM bridge_jobs j LEFT JOIN users u ON u.id = j.requested_by`

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (*Job, error) {
	var (
		j                        Job
		lib, by                  sql.NullInt64
		path, name               sql.NullString
		lease, claimed, finished sql.NullString
		params, progress, result string
	)
	if err := row.Scan(&j.ID, &j.Kind, &lib, &path, &params, &j.State, &progress, &result, &j.Error,
		&by, &name, &j.Priority, &j.Attempts, &lease, &j.CancelRequested,
		&j.CreatedAt, &j.UpdatedAt, &claimed, &finished, &j.FeedRedacted); err != nil {
		return nil, err
	}
	if lib.Valid {
		j.LibraryID = &lib.Int64
	}
	if path.Valid {
		j.Path = &path.String
	}
	if by.Valid && name.Valid {
		j.RequestedBy = &Requester{ID: by.Int64, Username: name.String}
	}
	j.Params, j.Progress, j.Result = json.RawMessage(params), json.RawMessage(progress), json.RawMessage(result)
	j.LeaseUntil, j.ClaimedAt, j.FinishedAt = nullable(lease), nullable(claimed), nullable(finished)
	j = readableJob(j)
	return &j, nil
}

// readableJob is j with its stored params, progress and result that aren't
// JSON (a damaged row) set to null, and its params flagged so
// (ParamsUnreadable): encoding a json.RawMessage that isn't JSON fails, and a
// handler that has written its 200 by then answers an empty body. Every job
// read (scanJob) goes through it, so no response path (the console's list,
// the estimate's open job, a queue's dedupe answer, the Bridge's claim) can
// carry one; a claim ends a job so flagged (claimPriority).
func readableJob(j Job) Job {
	if j.Params != nil && !json.Valid(j.Params) {
		j.Params, j.ParamsUnreadable = nil, true
	}
	if j.Progress != nil && !json.Valid(j.Progress) {
		j.Progress = nil
	}
	if j.Result != nil && !json.Valid(j.Result) {
		j.Result = nil
	}
	return j
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// rowQuerier is the reader pool or a write transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowsQuerier is the reader pool or a write transaction, for many rows.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// getJob reads one job through q (inside a write transaction: the writer's
// view, not the reader pool's).
func getJob(ctx context.Context, q rowQuerier, id int64) (*Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ErrNotFound is a job id with no job.
var ErrNotFound = errors.New("job not found")

// DedupeKey is a job's identity among the open jobs: its kind, library and path.
func DedupeKey(kind string, libraryID *int64, path *string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('|')
	if libraryID != nil {
		b.WriteString(strconv.FormatInt(*libraryID, 10))
	}
	b.WriteByte('|')
	if path != nil {
		b.WriteString(*path)
	}
	return b.String()
}

// jobKey is j's dedupe key: DedupeKey, extended by its kind's suffix when the
// kind has one.
func jobKey(j NewJob) string {
	key := DedupeKey(j.Kind, j.LibraryID, j.Path)
	if suffix := kinds[j.Kind].suffix; suffix != nil {
		key += suffix(j)
	}
	return key
}

// NewJob is a validated request for a job, after the transport resolved its target.
type NewJob struct {
	Kind      string
	LibraryID *int64
	// Path is the book's indexed path (nil: the job is about the library itself).
	Path        *string
	Params      json.RawMessage
	RequestedBy int64
	Priority    int
	// Check, when set, refuses the job with its error, run in Enqueue's
	// transaction once lapsed leases are ended (before the open job of the
	// kind is looked up, so it refuses even when one is open), so a write
	// that would refuse it can't land between the caller's own look and the
	// insert: chapters.detect's catalog.DetectRefusal (server#471).
	Check        func(context.Context, *sql.Tx) error
	feedRedacted string
	fetch        *FetchOptions
	// adoptOnly: only an open job with the same dedupe key is returned;
	// ErrNoNewRequest instead of inserting one.
	adoptOnly bool
}

// ErrKindUnavailable is a job of a kind the paired Bridge doesn't run (or no
// Bridge has reported yet): a 409.
var ErrKindUnavailable = errors.New("the paired bridge doesn't run that kind")

// ErrLibraryNotWritable is a job that writes into a library the paired Bridge's
// last heartbeat doesn't report as mapped and writable (or doesn't report at
// all): a 409 library_not_writable.
var ErrLibraryNotWritable = errors.New("the bridge can't write to that library")

// ErrGenerateOpen is a sidecars.describe, chapters.detect or
// sidecars.contribute job for a book with a sidecars.generate job still open:
// that job writes the description, and finds the chapters of a book without
// any, itself, so a second writer is refused, and it replaces the characters
// and recaps a contribute job would file (a 409 generate_open). A generate job
// is never refused for an open describe job: queueing it cancels that one
// (cancelOpenTx), and the generate job writes a description of its own.
var ErrGenerateOpen = errors.New("a generate job is open for the book")

// ErrDetectOpen is a sidecars.generate job for a book with a chapters.detect
// job queued or working (cancelling that one would throw its transcription
// away, and a generate job queued once it is done reuses it; a parked one is
// cancelled instead), or a sidecars.describe job for a book with one open (both
// rewrite the book's bridge.json): a 409 detect_open.
var ErrDetectOpen = errors.New("a chapter detection job is open for the book")

// ErrContributeOpen is a sidecars.generate job for a book with a
// sidecars.contribute job queued or working: the generate run would replace the
// characters and recaps being filed (a 409 contribute_open). A parked contribute
// job (a GitHub sign-in refused, say), or a queued one held while the
// contributions module is off (no claim hands it out), is cancelled by the new
// generate job instead, which can file its own results.
var ErrContributeOpen = errors.New("a contribute job is open for the book")

// ErrReadalongOpen is a sidecars.generate job for a book with a queued or
// working readalong.align job (a parked one is cancelled by it instead), or a
// chapters.detect job for a book with any open one: both transcribe the
// book's audio on the Bridge, which runs one engine book per audio source.
var ErrReadalongOpen = errors.New("a read-along job is open for the book")

// ErrDescribeOpen is a chapters.detect job for a book with a sidecars.describe
// job still open: both rewrite the book's bridge.json on the Bridge, so the
// second is refused (a 409 describe_open).
var ErrDescribeOpen = errors.New("a description job is open for the book")

// ErrChaptersChosen is a chapters.detect job for a book whose chapter source
// an admin chose (the files' or the community's): the chapters it would find
// stand in only when none was chosen, so they would never be used (a 409
// chapters_chosen). catalog.DetectRefusal answers it, as the catalog holds
// the choice; making one cancels a queued or parked job (CancelChaptersChosen).
var ErrChaptersChosen = errors.New("an admin chose the book's chapter source")

// ErrChaptersEdited is a chapters.detect job for a book whose chapters an
// admin edited, while the edit applies (catalog.ChaptersFromEdited): an edit
// stands over every other source, so a found list would sit dormant under it
// (a 409 chapters_edited). catalog.DetectRefusal answers it, as the catalog
// holds the edit; saving one cancels a queued or parked job (CancelChaptersEdited).
var ErrChaptersEdited = errors.New("an admin edited the book's chapters")

// ErrCannotContribute is a job that files to the community (ModuleFor names
// ModuleContributions: sidecars.contribute, meta.correct, a sidecars.generate
// job with contribute "issue") for a Bridge whose last heartbeat doesn't say it
// can (Status.CanContribute; none sent counts as can't): a 409
// cannot_contribute.
var ErrCannotContribute = errors.New("the bridge can't contribute to the community")

// Writable reports whether the Bridge last reported library id as mapped and
// writable, the condition for a job that writes into it.
func (st Status) Writable(id int64) bool {
	l := st.Library(id)
	return l != nil && l.Mapped && l.Writable
}

// checkNewJob is every check a new job passes before its key is looked up:
// checkNew, then the library's kind (checkLibraryKind).
func (s *Service) checkNewJob(ctx context.Context, j *NewJob) error {
	if err := checkNew(j); err != nil {
		return err
	}
	return s.checkLibraryKind(ctx, *j)
}

// checkNew validates a request against the kind: known, its params well formed
// (normalised in place), a library when the kind needs one and a path only when
// it allows one.
func checkNew(j *NewJob) error {
	spec, ok := kinds[j.Kind]
	if !ok {
		return invalid("unknown kind %q", j.Kind)
	}
	if spec.needsLibrary && j.LibraryID == nil {
		return invalid("library_id is required")
	}
	if !spec.needsLibrary && (j.LibraryID != nil || j.Path != nil) {
		return invalid("this kind takes no library or path")
	}
	if j.Path != nil && spec.book == bookNone {
		return invalid("this kind takes no path")
	}
	if spec.book == bookShow && !validShowPath(j.Path) {
		return invalid("path must name one show")
	}
	if spec.book == bookEpisode && (j.Path == nil || !validEpisodePath(*j.Path)) {
		return invalid("path must name an episode")
	}
	if j.Path == nil && spec.book == bookRequired {
		return invalid("path is required")
	}
	if len(j.Params) > spec.paramsCap() {
		return invalid("params too large")
	}
	params, err := spec.params(j.Params)
	if err != nil {
		return err
	}
	j.Params = params
	return nil
}

// Enqueue queues j and returns it, or the open job with the same kind, library
// and path when there is one (created false). The kind must be one the server
// knows (ErrInvalid otherwise, as for bad params) and the paired Bridge runs
// (ErrKindUnavailable); it may be offline, and the job waits. A kind that writes
// into the library is queued only when the last heartbeat reports the library
// mapped and writable (ErrLibraryNotWritable), a job that files to the
// community only when it says the Bridge can (ErrCannotContribute), and any
// job only while no open job of another kind refuses it (kindSpec's
// refusedWhileOpen: ErrGenerateOpen, ErrDetectOpen, ErrDescribeOpen,
// ErrContributeOpen, ErrReadalongOpen); the open job of its own kind is still returned either way.
// j.Check, when set, refuses it in the insert's transaction once the kind is
// known to run, ahead of the open job and the other refusals
// (chapters.detect's catalog.DetectRefusal).
// The caller has resolved the library and the book (j.Path is the book's
// indexed path) and checked the job's module is on (ModuleFor).
func (s *Service) Enqueue(ctx context.Context, j NewJob) (*Job, bool, error) {
	if j.Kind == KindPodcastFetch && j.fetch == nil {
		return s.EnsurePodcastFetch(ctx, j, FetchOptions{Retry: true})
	}
	if err := s.checkNewJob(ctx, &j); err != nil {
		return nil, false, err
	}
	return s.enqueueChecked(ctx, j)
}

// enqueueChecked is Enqueue for a job checkNewJob has already passed (params
// normalised): Subscribe checks an adopt-only request before it looks for the
// open one, and the check isn't made twice.
func (s *Service) enqueueChecked(ctx context.Context, j NewJob) (job *Job, created bool, err error) {
	var h *Heartbeat
	if j.fetch == nil {
		if h, err = s.LastHeartbeat(ctx); err != nil {
			return nil, false, err
		}
		if h == nil || !h.Offers(j.Kind) {
			return nil, false, ErrKindUnavailable
		}
	}
	var refuse error
	switch {
	case j.fetch != nil:
		// New fetch authorization is checked in the writer transaction below.
	case kinds[j.Kind].writes && !h.Writable(*j.LibraryID):
		refuse = ErrLibraryNotWritable
	case ModuleFor(j.Kind, j.Params) == ModuleContributions && !h.CanContribute():
		refuse = ErrCannotContribute
	}
	key := jobKey(j)
	var exp expiry
	identityBusy, promoted := false, false
	err = s.db.WithTx(ctx, "bridge.Enqueue", func(tx *sql.Tx) error {
		now := s.ts()
		var err error
		if exp, err = expireTx(ctx, tx, now); err != nil {
			return err
		}
		if j.Check != nil {
			if err := j.Check(ctx, tx); err != nil {
				return err
			}
		}
		var id int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM bridge_jobs WHERE dedupe_key = ? AND state IN `+OpenStatesSQL, key).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			job, err = getJob(ctx, tx, id)
			if err != nil {
				return err
			}
			if j.Kind == KindPodcastSubscribe && subscribePrivate(job.Params) != subscribePrivate(j.Params) {
				return ErrPrivacyConflict
			}
			if !podcastWork(j.Kind) || samePodcastIdentity(job.Params, j.Params) {
				if j.fetch != nil {
					if err := checkPodcastTarget(ctx, tx, j); err != nil {
						return err
					}
				}
				// A demo stream (NoNew) watches the open fetch but never raises it:
				// it may not ask for disk work, the household's queue included.
				if j.fetch == nil && !kinds[j.Kind].promotes || j.fetch != nil && j.fetch.NoNew {
					return nil
				}
				// Promotion only ever raises the priority; an unchanged row
				// is not rewritten and wakes no claim.
				res, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET priority=?,updated_at=? WHERE id=? AND priority<?`, j.Priority, now, id, j.Priority)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil {
					return err
				} else if n > 0 {
					promoted = true
					job.Priority = j.Priority
					job.UpdatedAt = now
				}
				// A job not claimed yet runs with the newest request's params
				// (both normalised by checkNew): a Check now after the
				// account's template changed plans by the new one.
				if kinds[j.Kind].promotes && job.State == StateQueued && !bytes.Equal(job.Params, j.Params) {
					res, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET params=?,updated_at=? WHERE id=? AND state=?`,
						string(j.Params), now, id, StateQueued)
					if err != nil {
						return err
					}
					if n, err := res.RowsAffected(); err != nil {
						return err
					} else if n > 0 {
						// Params that couldn't be read (a damaged row) are
						// replaced with them: the job is no longer flagged.
						job.Params, job.UpdatedAt, job.ParamsUnreadable = j.Params, now, false
					}
				}
				return nil
			}
			// Only a request naming the current identity may cancel the open
			// work: a stale one must never cancel the current show's job.
			if err := checkPodcastTarget(ctx, tx, j); err != nil {
				return err
			}
			if j.fetch != nil && j.fetch.NoNew {
				// A demo stream changes nothing of the queue: it queues no fetch
				// of its own, so it cancels none either, a running one included.
				return ErrFetchDemo
			}
			if err := cancelOpenTx(ctx, tx, key, now, CancelReplacedRequest); err != nil {
				return err
			}
			if job.State != StateQueued && job.State != StateNeedsAttention {
				identityBusy = true
				return nil
			}
		} else if err := checkPodcastTarget(ctx, tx, j); err != nil {
			return err
		}
		if j.adoptOnly {
			return ErrNoNewRequest
		}
		if j.fetch != nil {
			h, err := readHeartbeat(ctx, tx)
			if err != nil {
				return err
			}
			refuse = s.fetchRefusal(h, *j.LibraryID, j.fetch.NoNew)
		}
		if refuse != nil {
			return refuse
		}
		if j.fetch != nil {
			if !j.fetch.Retry {
				cooling, err := fetchCooling(ctx, tx, j, s.now())
				if err != nil {
					return err
				}
				if cooling {
					return ErrFetchCooldown
				}
			}
			if err := checkFetchLimit(ctx, tx, j.RequestedBy); err != nil {
				return err
			}
		}
		if j.Kind == KindPodcastSubscribe {
			if err := checkOpenRequests(ctx, tx, KindPodcastSubscribe, j.RequestedBy, OpenStatesSQL, ErrTooManyRequests); err != nil {
				return err
			}
		}
		if podcastWork(j.Kind) {
			if err := s.prunePolls(ctx, tx, j); err != nil {
				return err
			}
		}
		if j.Kind == KindAudiblePoll || j.Kind == KindAudibleSync {
			if err := pruneAudibleEnded(ctx, tx, j.Kind, key); err != nil {
				return err
			}
		}
		spec := kinds[j.Kind]
		b, err := s.blocker(ctx, tx, j.Kind, j.LibraryID, j.Path)
		if err != nil {
			return err
		}
		if b != nil {
			return spec.refusedWhileOpen[b.Kind]
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO bridge_jobs(kind, library_id, rel_path, params, requested_by, priority, dedupe_key, created_at, updated_at,feed_redacted)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?,?)`,
			j.Kind, j.LibraryID, j.Path, string(j.Params), requesterID(j.RequestedBy), j.Priority, key, now, now, j.feedRedacted)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		created = true
		// The generate run writes the description too, from fresher notes, and
		// finds the chapters itself: a describe job open for the book, and a
		// parked detect job (a working one refused it above), give way (at once
		// when no Bridge is on it, else at its next update), so the two never
		// race for the book's files. A parked or held contribute job gives way
		// too (givesWay): the run replaces what it was sharing.
		for kind, reason := range spec.cancelsOpen {
			if err := cancelOpenTx(ctx, tx, DedupeKey(kind, j.LibraryID, j.Path), now, reason); err != nil {
				return err
			}
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if identityBusy {
		s.wake.fire()
		s.settle(ctx, exp) // the expiry committed with the cancel intent
		return nil, false, ErrIdentityChanged
	}
	if created || promoted {
		s.wake.fire()
	}
	s.settle(ctx, exp)
	return job, created, nil
}

// Job returns one job, after expiring a lapsed lease (so the console shows what
// the Bridge is really doing).
func (s *Service) Job(ctx context.Context, id int64) (*Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	return getJob(ctx, s.db, id)
}

// Filter narrows List.
type Filter struct {
	LibraryID *int64
	Path      *string
	State     string
	Kind      string
	// Before is the previous page's last id (0: the first page).
	Before int64
	// Limit is the page size (1 to MaxListLimit; anything else DefaultListLimit).
	Limit int
}

// List returns jobs newest first, and the id to pass as Before for the next
// page (0 on the last page), after expiring lapsed leases.
func (s *Service) List(ctx context.Context, f Filter) (jobs []Job, nextBefore int64, err error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, 0, err
	}
	var (
		where []string
		args  []any
	)
	if f.Kind != "" {
		where, args = append(where, "j.kind = ?"), append(args, f.Kind)
	} else {
		where, args = append(where, "j.kind <> ?"), append(args, KindPodcastPoll)
	}
	if f.LibraryID != nil {
		where, args = append(where, "j.library_id = ?"), append(args, *f.LibraryID)
	}
	if f.Path != nil {
		where, args = append(where, "j.rel_path = ?"), append(args, *f.Path)
	}
	if f.State != "" {
		where, args = append(where, "j.state = ?"), append(args, f.State)
	}
	if f.Before > 0 {
		where, args = append(where, "j.id < ?"), append(args, f.Before)
	}
	q := `SELECT ` + jobColumns + jobFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > MaxListLimit {
		limit = DefaultListLimit
	}
	// One row more than the page says whether there is a next one.
	q += ` ORDER BY j.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	jobs = []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, RedactedJob(*j))
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(jobs) > limit {
		jobs = jobs[:limit]
		nextBefore = jobs[limit-1].ID
	}
	return jobs, nextBefore, nil
}

// lapsedSQL picks the jobs whose lease ran out (?1 is now).
const lapsedSQL = `state IN ` + claimedStatesSQL + ` AND lease_until < ?1`

// expireSQL ends every lease that ran out (?1 is now): a job whose cancel was
// asked for is cancelled, one on its last attempt (?2) failed (both keep what
// the dead attempt reported), the rest queued again with the dead attempt's
// progress, result and error dropped, so a waiting job doesn't read as still
// under way.
const expireSQL = `UPDATE bridge_jobs SET
	    state = CASE WHEN cancel_requested = 1 THEN 'cancelled'
	                 WHEN attempts >= ?2 THEN 'failed'
	                 ELSE 'queued' END,
	    error = CASE WHEN cancel_requested = 1 THEN error
	                 WHEN attempts >= ?2 THEN 'the Bridge stopped reporting on every attempt'
	                 ELSE '' END,
	    progress = CASE WHEN cancel_requested = 1 OR attempts >= ?2 THEN progress ELSE '{}' END,
	    result = CASE WHEN cancel_requested = 1 OR attempts >= ?2 THEN result ELSE '{}' END,
	    finished_at = CASE WHEN cancel_requested = 1 OR attempts >= ?2 THEN ?1 ELSE NULL END,
	    lease_until = NULL, updated_at = ?1
	  WHERE ` + lapsedSQL

// expiry is what expireTx ended: how many leases, and the jobs it failed (the
// lease of their last attempt), each with the state it was in.
type expiry struct {
	n      int64
	failed []transition
}

// transition is a job's move out of the state from, as OnTransition hears it.
type transition struct {
	job  *Job
	from string
}

// expireTx ends the leases that ran out, inside a write transaction, and says
// what it ended. Every transaction that reads or changes job states runs it
// first, so none acts on a dead claim. A Service method whose transaction ended
// any settles them once it commits: the waiting claims wake, so a requeued job
// is picked up at once rather than when a claim's wait runs out, and an admin
// hears about a failed one. (MoveJobs, in the catalog's move, doesn't run it:
// nothing there could settle what it ended.)
func expireTx(ctx context.Context, tx *sql.Tx, now string) (expiry, error) {
	var e expiry
	failing, err := lastAttemptsLapsed(ctx, tx, now)
	if err != nil {
		return e, err
	}
	res, err := tx.ExecContext(ctx, expireSQL, now, MaxAttempts)
	if err != nil {
		return e, err
	}
	if e.n, err = res.RowsAffected(); err != nil {
		return e, err
	}
	for _, t := range failing {
		if t.job, err = getJob(ctx, tx, t.job.ID); err != nil {
			return e, err
		}
		e.failed = append(e.failed, t)
	}
	return e, nil
}

// lastAttemptsLapsed is the jobs expireSQL is about to fail, read before it
// runs (the update keeps no trace of the state each was in): their ids, with
// the job itself filled in only once the update is done.
func lastAttemptsLapsed(ctx context.Context, tx *sql.Tx, now string) ([]transition, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM bridge_jobs
		  WHERE `+lapsedSQL+` AND (cancel_requested = 1 OR attempts >= ?2)`, now, MaxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []transition
	for rows.Next() {
		t := transition{job: &Job{}}
		if err := rows.Scan(&t.job.ID, &t.from); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// settle follows a committed transaction whose expireTx ended e: it wakes the
// waiting claims for the jobs it requeued and reports the jobs it failed.
func (s *Service) settle(ctx context.Context, e expiry) {
	if e.n > 0 {
		s.wake.fire()
	}
	for _, t := range e.failed {
		s.report(ctx, t.job, t.from, "")
	}
}

// report tells OnTransition about job's move out of from (fromCode: the park
// code from named, "" for none) when it is news (Notable, and a park not
// announced already: parkNews), on a context the caller's leaving doesn't
// cancel: the move has committed, and the admin should hear about it either way.
func (s *Service) report(ctx context.Context, job *Job, from, fromCode string) {
	if job.Kind == KindPodcastFetch && job.LibraryID != nil && job.Path != nil {
		s.NotifyPodcast(*job.LibraryID, *job.Path)
	}
	s.reportEnded(ctx, job, from)
	if s.OnTransition != nil && job.Notable(from, fromCode) && s.parkNews(job) {
		committed, cancel := committedContext(ctx)
		defer cancel()
		s.OnTransition(committed, *job, from)
	}
}

// expireDue is expireTx for the read paths: it takes the one writer only when a
// lease has actually run out (a reader-pool check on idx_bridge_jobs_lease
// first), so the console's polls don't queue behind scans for the writer.
func (s *Service) expireDue(ctx context.Context) error {
	now := s.ts()
	if due, err := s.leaseLapsed(ctx, now); err != nil || !due {
		return err
	}
	var exp expiry
	err := s.db.WithTx(ctx, "bridge.expire", func(tx *sql.Tx) error {
		var err error
		exp, err = expireTx(ctx, tx, now)
		return err
	})
	if err != nil {
		return err
	}
	s.settle(ctx, exp)
	return nil
}

// ErrNoHeartbeat is a claim from a Bridge that hasn't reported yet (a 409): the
// server queues for the kinds its heartbeat names.
var ErrNoHeartbeat = errors.New("the bridge hasn't reported yet")

// ClaimFor hands the Bridge the next queued job it can run, waiting up to wait
// (at most MaxWait) for one; nil when none came or ctx ended. The kinds are those
// its last heartbeat names that the server knows, narrowed to asked when it names
// any (the kinds its free lanes run); with none left it answers at once. token is
// the request's bridge token: once a pairing or an unpair revoked it (the long
// poll outlives the request's authentication), no job is handed out on it
// (ErrNotPaired).
func (s *Service) ClaimFor(ctx context.Context, token int64, asked []string, wait time.Duration) (*Job, error) {
	return s.ClaimWith(ctx, token, asked, wait, ClaimOptions{})
}

// ClaimOptions are a claim's optional terms, each one the Bridge asked for.
type ClaimOptions struct {
	// MinPriority is the priority floor (min_priority, negotiated through the
	// heartbeat's accepts): nil for none.
	MinPriority *int32
	// ShareOff: the Bridge honours a job's ShareOff (share_off=1), so a
	// sidecars.generate job that would also share is handed out while the
	// Contributions module is off, flagged to keep its results local, rather
	// than held (nextQueued). An older Bridge sends none: such a job stays
	// held for it, as it would share anyway. Unlike MinPriority it is sent
	// without the heartbeat's accepts naming it: a server that ignores it
	// holds the job and never answers share_off, as before.
	ShareOff bool
}

// ClaimWith is ClaimFor with the claim's options (ClaimOptions).
func (s *Service) ClaimWith(ctx context.Context, token int64, asked []string, wait time.Duration, opt ClaimOptions) (*Job, error) {
	h, err := s.LastHeartbeat(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, ErrNoHeartbeat
	}
	var ks []string
	for _, k := range h.Kinds {
		if Known(k) && (len(asked) == 0 || slices.Contains(asked, k)) {
			ks = append(ks, k)
		}
	}
	if len(ks) == 0 {
		return nil, nil
	}
	job, err := s.waitClaim(ctx, token, ks, wait, opt)
	if job != nil {
		s.flagShareOff(job)
	}
	return job, err
}

// claim hands the Bridge holding token the next queued job of one of kinds
// (highest priority, then oldest) that isn't held (nextQueued), leased for
// Lease, or nil when there is none; ErrNotPaired once token is no longer the
// paired Bridge's. An idle claim (claimDue false) answers from the reader pool
// and writes nothing.
func (s *Service) claimStored(ctx context.Context, token int64, kinds []string, opt ClaimOptions) (*Job, error) {
	due, hand, err := s.claimDue(ctx, token, kinds, opt)
	if err != nil || !due {
		return nil, err
	}
	// A claim due only to end a lapsed lease or a lost request (no job of kinds
	// queued) ends them in an ordinary commit (server#260): only leasing a job
	// needs the fsync, its attempt number never handed out twice. The jobs it
	// put back may be this claim's to lease, so it looks again.
	if !hand {
		if _, err := s.claimTx(ctx, s.db.WithTx, token, nil, ClaimOptions{}, false); err != nil {
			return nil, err
		}
		if id, err := s.nextQueued(ctx, s.db, kinds, opt); err != nil || id == 0 {
			return nil, err
		}
	}
	return s.claimTx(ctx, s.db.WithDurableTx, token, kinds, opt, true)
}

// claimTx is a claim's transaction, run by withTx: it ends the lapsed leases
// (expireTx) and the lost private requests (expirePrivateTx) and, with lease,
// leases the next queued job of kinds (nil when none). Each write has its check
// in claimDue, or an idle claim skips it.
func (s *Service) claimTx(ctx context.Context, withTx func(context.Context, string, func(*sql.Tx) error) error,
	token int64, kinds []string, opt ClaimOptions, lease bool) (*Job, error) {
	var exp expiry
	var job *Job
	err := withTx(ctx, "bridge.Claim", func(tx *sql.Tx) error {
		if err := bridgeTokenLive(ctx, tx, token); err != nil {
			return err
		}
		now := s.ts()
		var err error
		if exp, err = expireTx(ctx, tx, now); err != nil {
			return err
		}
		expired, err := s.expirePrivateTx(ctx, tx, now)
		if err != nil {
			return err
		}
		for i := range expired {
			exp.failed = append(exp.failed, transition{job: &expired[i], from: StateQueued})
		}
		if !lease {
			return nil
		}
		id, err := s.nextQueued(ctx, tx, kinds, opt)
		if err != nil || id == 0 {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bridge_jobs SET state = 'claimed', attempts = attempts + 1, lease_until = ?, claimed_at = ?,
			        updated_at = ? WHERE id = ?`, at(s.now().Add(Lease)), now, now, id); err != nil {
			return err
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.settle(ctx, exp) // the other waiting claims wake for the jobs it requeued
	return job, nil
}

// claimDue reports whether a claim has anything to write, read on the reader
// pool: a lease that ran out (expireTx's, of any kind), a private follow request
// whose feed URL this server no longer holds (expirePrivateTx's), or a job
// nextQueued would hand out for kinds that isn't such a request (hand). A paired Bridge long-polls on two loops,
// 4 to 6 claims a minute when idle; without this each took the one writer and
// moved the content generation (store.DB.ContentGeneration), emptying every cache keyed
// on it, to change nothing. ErrNotPaired as the claim's own check, which the
// claim's transaction repeats. A write landing after these reads is no loss: an
// enqueue or requeue wakes the waiting claims (waitClaim takes its wake channel
// before it claims), and a lease that lapses later is due at the next claim.
func (s *Service) claimDue(ctx context.Context, token int64, kinds []string, opt ClaimOptions) (due, hand bool, err error) {
	if err := bridgeTokenLive(ctx, s.db, token); err != nil {
		return false, false, err
	}
	lost, err := s.lostPrivateRequests(ctx, s.db)
	if err != nil {
		return false, false, err
	}
	id, err := s.nextQueued(ctx, s.db, kinds, opt)
	switch {
	case err != nil:
		return false, false, err
	case id != 0 && !slices.Contains(lost, id):
		return true, true, nil
	case id != 0 || len(lost) > 0:
		// The next job is a lost request: failing it leases nothing, and the
		// ordinary commit's look after it finds the job behind it, if any.
		return true, false, nil
	}
	lapsed, err := s.leaseLapsed(ctx, s.ts())
	return lapsed, false, err
}

// bridgeTokenLive answers ErrNotPaired once token is no longer the paired
// Bridge's, read through q (the reader pool or the claim's transaction).
func bridgeTokenLive(ctx context.Context, q rowQuerier, token int64) error {
	var live bool
	if err := q.QueryRowContext(ctx, `SELECT `+auth.BridgeTokenLiveSQL, token).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrNotPaired
	}
	return nil
}

// leaseLapsed reports, on the reader pool (idx_bridge_jobs_lease), whether any
// lease ran out by now: whether expireTx has anything to do.
func (s *Service) leaseLapsed(ctx context.Context, now string) (bool, error) {
	var lapsed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bridge_jobs WHERE `+lapsedSQL+`)`, now).Scan(&lapsed)
	return lapsed, err
}

// maxOpenRequests caps one requester's open jobs of a capped kind.
const maxOpenRequests = 5

// checkOpenRequests answers over when by already has maxOpenRequests jobs of
// kind in states (an SQL list: OpenStatesSQL, or workingStatesSQL to leave
// parked ones out), inside the insertion's writer transaction.
func checkOpenRequests(ctx context.Context, tx *sql.Tx, kind string, by int64, states string, over error) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs WHERE kind=? AND requested_by=? AND state IN `+states, kind, by).Scan(&n); err != nil {
		return err
	}
	if n >= maxOpenRequests {
		return over
	}
	return nil
}

// ReleaseUnsentClaim returns an undelivered claim without spending an attempt.
// A job whose cancel was asked for is cancelled instead. A Bridge that read the
// claim after all still hears a cancel as one (cancelledUnder).
func (s *Service) ReleaseUnsentClaim(ctx context.Context, claim *Job) error {
	now := s.ts()
	return s.settleUnsentClaim(ctx, claim, "bridge.ReleaseUnsentClaim", `UPDATE bridge_jobs SET
		 state=CASE WHEN cancel_requested=1 THEN 'cancelled' ELSE 'queued' END,
		 attempts=attempts-1,lease_until=NULL,claimed_at=NULL,updated_at=?,
		 finished_at=CASE WHEN cancel_requested=1 THEN ? ELSE NULL END`+unsentClaimSQL, now, now)
}

// failUnsentClaim ends an undelivered claim the server could not fill, on its
// last attempt or when no attempt ever could, with reason as its error: at once
// rather than when its lease runs out, which would name the Bridge (expireSQL).
// A job whose cancel was asked for is cancelled instead.
func (s *Service) failUnsentClaim(ctx context.Context, claim *Job, reason string) error {
	now := s.ts()
	return s.settleUnsentClaim(ctx, claim, "bridge.failUnsentClaim", `UPDATE bridge_jobs SET
		 state=CASE WHEN cancel_requested=1 THEN 'cancelled' ELSE 'failed' END,
		 error=CASE WHEN cancel_requested=1 THEN error ELSE ? END,
		 lease_until=NULL,updated_at=?,finished_at=?`+unsentClaimSQL, reason, now, now)
}

// endUnfillableClaim ends claim, a job no claim could fill now (an
// audible.sync, fillAudibleSync, or any job whose params can't be read,
// claimPriority), with
// reason (failUnsentClaim) and logs its read error (readErr) at warn with msg
// and attrs, once it has ended. A settle that fails leaves the claim to its
// lease and answers both errors, so the claim's own error reports them.
func (s *Service) endUnfillableClaim(ctx context.Context, claim *Job, reason string, readErr error, msg string,
	attrs ...any) error {
	if err := s.failUnsentClaim(ctx, claim, reason); err != nil {
		return errors.Join(readErr, err)
	}
	s.Log.Warn(msg, append([]any{"job", claim.ID, "err", readErr}, attrs...)...)
	return nil
}

// unsentClaimSQL picks an undelivered claim (its id, attempts, lease and claim
// time follow the update's own args): the state, attempt and lease identify the
// untouched claim, as any Bridge report either changes its state or renews its
// lease, and makes the update a no-op.
const unsentClaimSQL = `
		 WHERE id=? AND state='claimed' AND attempts=? AND lease_until=? AND claimed_at=?`

// settleUnsentClaim runs update (ending in unsentClaimSQL, with its own args)
// on an undelivered claim. It runs detached from ctx's cancellation (the claim
// request may have gone) and bounded, so a database fault cannot hold the
// caller indefinitely.
func (s *Service) settleUnsentClaim(ctx context.Context, claim *Job, op, update string, args ...any) error {
	ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer stop()
	var job *Job
	err := s.db.WithTx(ctx, op, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, update,
			append(args, claim.ID, claim.Attempts, claim.LeaseUntil, claim.ClaimedAt)...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		job, err = getJob(ctx, tx, claim.ID)
		return err
	})
	if err == nil && job != nil {
		s.wake.fire()
		s.report(ctx, job, StateClaimed, "")
	}
	return err
}

// nextQueued is the id of the next queued job of one of kinds (highest
// priority, then oldest), 0 for none, passing over a held one: a job that needs
// a managed module that is off (held) waits, queued, until the module is on
// again. A generate job held only for its share (sharesAlong) is not held from
// a claim that honours ShareOff (opt.ShareOff): it runs, its results kept
// local. The queue is short, so the kinds are matched as read (the statement
// stays a constant).
func (s *Service) nextQueued(ctx context.Context, q rowsQuerier, kinds []string, opt ClaimOptions) (int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, kind, params FROM bridge_jobs WHERE state = 'queued' AND (? IS NULL OR priority >= ?) ORDER BY priority DESC, id`, opt.MinPriority, opt.MinPriority)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			kind   string
			params []byte
		)
		if err := rows.Scan(&id, &kind, &params); err != nil {
			return 0, err
		}
		if !slices.Contains(kinds, kind) {
			continue
		}
		if !s.held(kind, params) || opt.ShareOff && sharesAlong(kind, params) {
			return id, rows.Close() // closed before the claim's update: one connection
		}
	}
	return 0, rows.Err()
}

// waitClaim is claimPriority that waits up to wait (at most MaxWait) for a job to be
// queued when there is none, returning nil when none came or ctx ended (the
// caller left, or the server is stopping: a claim cut short by it committed
// nothing).
func (s *Service) waitClaim(ctx context.Context, token int64, kinds []string, wait time.Duration, opt ClaimOptions) (*Job, error) {
	wait = min(max(wait, 0), MaxWait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		woken := s.wake.wait()
		job, err := s.claimPriority(ctx, token, kinds, opt)
		if err != nil && ctx.Err() != nil {
			return nil, nil
		}
		if job != nil || err != nil {
			return job, err
		}
		select {
		case <-woken:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// Update is what the Bridge reports about a job it holds (PATCH
// /bridge/jobs/{id}). Attempt is the attempts value its claim returned, so a
// Bridge reporting on a claim that expired (and was claimed again) is refused.
// Fields left nil keep their value.
type Update struct {
	Attempt  int             `json:"attempt"`
	State    *string         `json:"state"`
	Progress json.RawMessage `json:"progress"`
	Result   json.RawMessage `json:"result"`
	Error    *string         `json:"error"`
}

// ErrNotHeld is an update for a job the caller no longer holds: it ended, was
// cancelled, its lease ran out and a later claim took it, or it is gone (with its
// library). A 409 not_held, which tells the Bridge to drop it.
var ErrNotHeld = errors.New("the job is not held by this claim")

// ErrCancelledWhileHeld is ErrNotHeld for a job that ended cancelled with no
// later claim than this one (never attempt 0, which is no claim): a parked job
// cancelled (by an admin, or by the server itself: Cancel*, cancelsOpen), which
// ends at once, as does a held one whose book moved onto another open job of
// its kind (MoveJobs), a working one whose lease ran out after the cancel was
// asked for, one this claim lost to a lapsed lease and that was cancelled
// while queued again, or one whose claim the server released as undelivered
// though the Bridge read it (ReleaseUnsentClaim) and that was cancelled then or
// while queued again (cancelledUnder). The 409 not_held then carries reason "cancelled", so the
// Bridge records the job cancelled rather than lost, which is how it ended
// (bridge#35, BRIDGE-04). errors.Is reads it as ErrNotHeld too.
var ErrCancelledWhileHeld = fmt.Errorf("%w: it was cancelled", ErrNotHeld)

// cancelledUnder reports whether job cur ended cancelled with no claim later
// than attempt (ErrCancelledWhileHeld). Attempt 0 is no claim: a job cancelled
// before any took it. The job's attempts are the claim's own, or one fewer
// when the server released the claim as undelivered (ReleaseUnsentClaim gives
// the attempt back) though the Bridge had read it, and it was cancelled then
// or while queued again: only a later claim takes the count past it.
func cancelledUnder(cur *Job, attempt int) bool {
	return cur.State == StateCancelled && attempt > 0 && (cur.Attempts == attempt || cur.Attempts == attempt-1)
}

// heldStates are the states the Bridge holds a job in (it may report on it);
// reportable are the states it may report.
var (
	heldStates = []string{StateClaimed, StateRunning, StateNeedsAttention}
	reportable = []string{StateRunning, StateDone, StateFailed, StateNeedsAttention, StateCancelled}
)

// checkUpdate bounds what the Bridge sends: progress and result are JSON
// objects (the request decoder already checked they are JSON), the error cleaned
// to printable bounded text, line breaks kept (cleanText: a failed job's report
// must land whatever its error quotes).
func checkUpdate(u *Update) error {
	for name, raw := range map[string]struct {
		v   json.RawMessage
		max int
	}{"progress": {u.Progress, maxProgressBytes}, "result": {u.Result, maxResultBytes}} {
		if raw.v == nil {
			continue
		}
		if len(raw.v) > raw.max {
			return invalid("%s too large", name)
		}
		if t := bytes.TrimSpace(raw.v); len(t) == 0 || t[0] != '{' {
			return invalid("%s must be a JSON object", name)
		}
	}
	if u.Error != nil {
		e := cleanText(*u.Error, maxErrorBytes, true)
		u.Error = &e
	}
	return nil
}

// UpdateHeld applies the Bridge's update to job id. The job must be held by
// this claim (u.Attempt) in a state the Bridge reports from, and the new state
// one it may report; a claimed or running job's lease is renewed, a parked
// one's dropped, a finished one's ended. It returns the job as stored, whose
// CancelRequested tells the Bridge an admin, or the server (a share whose
// module was turned off: stopsShare), asked it to stop. A move into a state an
// admin hears about is reported (OnTransition) once it commits.
func (s *Service) UpdateHeld(ctx context.Context, id int64, u Update) (*Job, error) {
	if err := checkUpdate(&u); err != nil {
		return nil, err
	}
	var (
		job      *Job
		from     string
		fromCode string
		exp      expiry
	)
	// A user-state transaction (server#258): a report that renews the lease or
	// reports progress (a running job's keepalive, its step and message)
	// changes no more than bridge_jobs' lease_until, updated_at and progress,
	// which no content cache reads, and leaves the content generation alone;
	// one that changes the job's state, result or error, or expires another
	// job's lease, moves it like any write (store's rule for bridge_jobs,
	// checked by value).
	err := s.db.WithUserStateTx(ctx, "bridge.UpdateHeld", func(tx *sql.Tx) error {
		now := s.ts()
		var err error
		if exp, err = expireTx(ctx, tx, now); err != nil {
			return err
		}
		cur, err := getJob(ctx, tx, id)
		if errors.Is(err, ErrNotFound) {
			// Gone with its library (or a restored database): no claim holds it.
			return ErrNotHeld
		}
		if err != nil {
			return err
		}
		if !slices.Contains(heldStates, cur.State) || cur.Attempts != u.Attempt {
			if cancelledUnder(cur, u.Attempt) {
				return ErrCancelledWhileHeld
			}
			return ErrNotHeld
		}
		if IsPodcast(cur.Kind) {
			sanitizePodcastUpdate(&u)
		}
		from = cur.State
		if from == StateNeedsAttention {
			fromCode = cur.ParkCode()
		}
		state := cur.State
		if u.State != nil {
			if !slices.Contains(reportable, *u.State) {
				return invalid("state %q from %q", *u.State, cur.State)
			}
			state = *u.State
		}
		var lease, finished any
		switch state {
		case StateClaimed, StateRunning:
			// A job that only files to the community stops sharing once the
			// Contributions module is off: it is asked to stop at this report,
			// with the server's reason, as a cancel an admin asked for is
			// (server#218). Any other job the Bridge holds carries on.
			if !cur.CancelRequested && s.stopsShare(cur.Kind) {
				if _, err := tx.ExecContext(ctx,
					`UPDATE bridge_jobs SET cancel_requested = 1, error = ? WHERE id = ?`, CancelModuleOff, id); err != nil {
					return err
				}
				cur.CancelRequested, cur.Error = true, CancelModuleOff
			}
			until := s.now().Add(Lease)
			// Even a fieldless acknowledgement in the claim's second must
			// distinguish its renewed lease from an undelivered response.
			if state == StateClaimed && cur.LeaseUntil != nil {
				previous, err := time.Parse(time.RFC3339, *cur.LeaseUntil)
				if err == nil && !until.Truncate(time.Second).After(previous) {
					until = previous.Add(time.Second)
				}
			}
			lease = at(until)
		case StateDone, StateFailed, StateCancelled:
			finished = now
		}
		// A field the update leaves out (nil) keeps its value.
		var progress, result, errText any
		if u.Progress != nil {
			if from == StateNeedsAttention && state == StateNeedsAttention {
				u.Progress = carryPark(u.Progress, cur.Progress)
			}
			progress = string(u.Progress)
		}
		if u.Result != nil {
			result = string(u.Result)
		}
		// The server's reason for a cancel it asked for (cancelOpenTx) stays
		// through the Bridge's reports, which always send their own error, while
		// the job works and when it ends cancelled. A job that finishes, fails or
		// parks anyway carries the Bridge's own error (or none), never the reason.
		serverCancel := cur.CancelRequested && IsCancelReason(cur.Error)
		switch {
		case serverCancel && (state == StateDone || state == StateFailed || state == StateNeedsAttention):
			errText = ""
			if u.Error != nil {
				errText = *u.Error
			}
		case u.Error != nil && !serverCancel:
			errText = *u.Error
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bridge_jobs SET state = ?, lease_until = ?, finished_at = ?, updated_at = ?,
			        progress = COALESCE(?, progress), result = COALESCE(?, result), error = COALESCE(?, error)
			  WHERE id = ?`, state, lease, finished, now, progress, result, errText, id); err != nil {
			return err
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.settle(ctx, exp)
	s.report(ctx, job, from, fromCode)
	s.flagShareOff(job)
	return job, nil
}

// transientParks are the park codes that are not news, ones the Bridge resumes
// by itself once they pass (telling an admin about every one would flood a
// phone): the agent's rate limit, a short outage of the agent container, a wait
// for an ASR worker (a laptop worker asleep: each book that reaches its
// transcription would park in turn), and share_unconfirmed (a generate-and-share
// job that couldn't ask this server, just before filing, whether its share is
// still on). Any other park waits on something a person sees to, or is long
// enough to hear about (a monthly budget spent until the month turns, a
// credential refused), whatever its retry_at says.
var transientParks = []string{"agent_rate_limited", "agent_unavailable", "asr_worker_waiting", "share_unconfirmed"}

// Notable reports whether a job's move from the state `from` (fromCode: the
// park code it named there, "" for none) is one the server tells admins about
// (notifications): INTO done or failed, or INTO needs_attention with a park
// that isn't transient (transientParks); never a report that leaves the state
// as it was (a running job's keepalive, a parked job's progress). A park that
// changes its code in place is news when the new one isn't transient: the
// Bridge reports a rate limit's retry that ran into the book's budget without a
// running in between, and that park now waits for a person. It judges the one
// move; the Service then announces a park once per outage (parkNews).
func (j *Job) Notable(from, fromCode string) bool {
	if kinds[j.Kind].quiet {
		return false
	}
	switch j.State {
	case StateDone, StateFailed:
		return from != j.State
	case StateNeedsAttention:
		code := j.ParkCode()
		if slices.Contains(transientParks, code) {
			return false
		}
		return from != j.State || code != fromCode
	}
	return false
}

// reannounceAfter is how long a park announced once stays quiet while the
// Bridge keeps parking the job again for the same reason (parkNews): a later,
// separate outage on the same claim is not silent for ever.
const reannounceAfter = 6 * time.Hour

// parkNews reports whether a Notable move is still news once the parks
// already announced are counted. A Bridge that resumes a parked job on a timer
// (its retry_at) and parks it again with the same code (contrib_unavailable
// while GitHub is down) is announced once per outage, not every ten minutes:
// a park is news again when its code changes to another that isn't transient
// (a transient park in between changes nothing), when it turns from waiting on
// a person to waiting on a timer or back under the same code (budget_exceeded:
// the book's budget, then the month's), on a new claim, or once
// reannounceAfter has passed since it was last announced. Any other Notable
// move (done, failed) is always news. The console shows the park either way.
func (s *Service) parkNews(j *Job) bool {
	if j.State != StateNeedsAttention {
		return true
	}
	now := s.now()
	seen := announcedPark{attempt: j.Attempts, code: j.ParkCode(), timed: parkTimed(j.Progress), at: now}
	s.announcedMu.Lock()
	defer s.announcedMu.Unlock()
	last, ok := s.announced[j.ID]
	if ok && last.attempt == seen.attempt && last.code == seen.code && last.timed == seen.timed &&
		now.Sub(last.at) < reannounceAfter {
		return false
	}
	// An entry past reannounceAfter quiets nothing: dropping it bounds the
	// map by the parks announced in the last six hours, whatever ends unseen.
	for id, p := range s.announced {
		if now.Sub(p.at) >= reannounceAfter {
			delete(s.announced, id)
		}
	}
	s.announced[j.ID] = seen
	return true
}

// parkTimed reports whether the park a progress names carries a retry_at: the
// Bridge resumes it by itself then, where one without waits for a person.
func parkTimed(progress json.RawMessage) bool {
	var p struct {
		Park *struct {
			RetryAt string `json:"retry_at"`
		} `json:"park"`
	}
	return json.Unmarshal(progress, &p) == nil && p.Park != nil && p.Park.RetryAt != ""
}

// forgetPark drops the park announced for a job that ended (parkNews).
func (s *Service) forgetPark(id int64) {
	s.announcedMu.Lock()
	delete(s.announced, id)
	s.announcedMu.Unlock()
}

// carryPark is a parked job's new progress with the stored park kept when it
// names none: a Bridge resuming the job after a restart reports progress
// before it hears its park again, and a park dropped and restored would read
// as two new parks (two "job failed" events) and blank the console's reason.
// A progress that names a park (or one that isn't an object) is kept as sent,
// and a merge past the progress cap keeps the stored progress (park, reason).
func carryPark(progress, stored json.RawMessage) json.RawMessage {
	park := parkOf(stored)
	if park == nil || parkOf(progress) != nil {
		return progress
	}
	var next map[string]json.RawMessage
	if json.Unmarshal(progress, &next) != nil || next == nil {
		return progress
	}
	next["park"] = park
	out, err := marshalUnescaped(next)
	if err != nil {
		return progress
	}
	if len(out) <= maxProgressBytes {
		return out
	}
	return stored
}

// marshalUnescaped is v as JSON without HTML escaping: what is stored is only
// ever read back as JSON, and escaped, a "<" or "&" would take six bytes.
func marshalUnescaped(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// parkOf is the park a progress object names, nil when it names none: absent,
// null, not an object, or a park whose code reads empty (parkCode, the reading
// Notable and the console use, so a codeless park is never carried or news).
func parkOf(progress json.RawMessage) json.RawMessage {
	var p struct {
		Park json.RawMessage `json:"park"`
	}
	if parkCode(progress) == "" || json.Unmarshal(progress, &p) != nil {
		return nil
	}
	return p.Park
}

// ParkCode is the code of the park a job's progress names (progress.park.code,
// sidecars.generate's, cleaned), "" when it names none or can't be read.
func (j *Job) ParkCode() string { return parkCode(j.Progress) }

func parkCode(progress json.RawMessage) string {
	var p struct {
		Park *struct {
			Code string `json:"code"`
		} `json:"park"`
	}
	if json.Unmarshal(progress, &p) != nil || p.Park == nil {
		return ""
	}
	return cleanText(p.Park.Code, maxShortText, false)
}

// Blocker returns the open job of another kind that refuses a new job of kind
// for the book now (kindSpec's refusedWhileOpen: a queued or working chapter
// detection for a generate job, say), after expiring a lapsed lease; nil when
// none does.
func (s *Service) Blocker(ctx context.Context, kind string, libraryID int64, path string) (*Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	return s.blocker(ctx, s.db, kind, &libraryID, &path)
}

// OpenJob returns the open job of kind for a book (its dedupe key), or nil when
// there is none, after expiring lapsed leases (a console read).
func (s *Service) OpenJob(ctx context.Context, kind string, libraryID int64, path string) (*Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	return openJob(ctx, s.db, DedupeKey(kind, &libraryID, &path))
}

// ErrFinished is a cancel for a job that already ended (a 409).
var ErrFinished = errors.New("the job has already finished")

// Cancel cancels job id: at once when it is queued or parked (no Bridge is
// working on it), else by asking the Bridge (cancel_requested), which stops at
// its next update; a lease that runs out on such a job cancels it then.
func (s *Service) Cancel(ctx context.Context, id int64) (*Job, error) {
	var (
		job  *Job
		from string
		exp  expiry
	)
	err := s.db.WithTx(ctx, "bridge.Cancel", func(tx *sql.Tx) error {
		now := s.ts()
		var err error
		if exp, err = expireTx(ctx, tx, now); err != nil {
			return err
		}
		cur, err := getJob(ctx, tx, id)
		if err != nil {
			return err
		}
		from = cur.State
		switch cur.State {
		case StateQueued, StateNeedsAttention:
			_, err = tx.ExecContext(ctx,
				`UPDATE bridge_jobs SET state = 'cancelled', cancel_requested = 1, lease_until = NULL,
				        finished_at = ?1, updated_at = ?1 WHERE id = ?2`, now, id)
		case StateClaimed, StateRunning:
			_, err = tx.ExecContext(ctx,
				`UPDATE bridge_jobs SET cancel_requested = 1, updated_at = ? WHERE id = ?`, now, id)
		default:
			return ErrFinished
		}
		if err != nil {
			return err
		}
		job, err = getJob(ctx, tx, id)
		return err
	})
	if err == nil {
		s.settle(ctx, exp)
		s.report(ctx, job, from, "")
	}
	return job, err
}

// The reasons the server records as a job's error when it cancels the job
// itself, not an admin (the console words a cancel by them, BRIDGE-02, and
// keeps a copy of these strings: admin-ui generate-model.ts, checked by
// TestCancelReasonsMatchConsole).
const (
	// CancelMoved: MoveJobs, the book moved onto another open job of its kind.
	CancelMoved = "the book moved onto another job of this kind"
	// CancelReplacedRequest: a podcast request for a newer identity of the
	// same work replaced the open one (Enqueue).
	CancelReplacedRequest = "a newer request replaced it"
	// CancelModuleOff: the Contributions module was turned off while the
	// Bridge worked on a job that only files to the community (stopsShare).
	CancelModuleOff = "its module was turned off"
	// CancelChaptersEdited: an admin saved the book's own chapters
	// (catalog.SaveChapterEdit) while a chapters.detect job was queued or
	// parked: the list it would find sits dormant under the edit
	// (ErrChaptersEdited). One the Bridge is working on runs on
	// (CancelWaitingBookJob).
	CancelChaptersEdited = "the book's chapters were edited in the console"
	// CancelChaptersChosen: an admin chose the book's chapter source (the
	// files' or the community's) while a chapters.detect job was queued or
	// parked: the list it would find is not used under the choice
	// (ErrChaptersChosen).
	CancelChaptersChosen = "a chapter source was chosen for the book"
)

// IsCancelReason reports whether text is one of the server's own cancel
// reasons (cancelReasons).
func IsCancelReason(text string) bool {
	return slices.Contains(cancelReasons(), text)
}

// cancelReasons is every reason IsCancelReason knows, the list the console's
// copy (SERVER_CANCEL_REASONS) is checked against (TestCancelReasonsMatchConsole).
// The kinds are fixed at init, so it is built once.
var cancelReasons = sync.OnceValue(func() []string {
	out := []string{CancelMoved, CancelReplacedRequest, CancelModuleOff, CancelChaptersEdited, CancelChaptersChosen,
		CancelAudibleRemoved, CancelAudiblePaused, CancelAudibleSyncOff}
	for _, spec := range kinds {
		out = slices.AppendSeq(out, maps.Values(spec.cancelsOpen))
	}
	return out
})

// cancelOpenTx cancels the open job with dedupe key key, inside tx, as Cancel
// does: at once when it is queued or parked, else by asking the Bridge
// (cancel_requested). reason is its error. No open job is no change. Each
// UPDATE repeats idx_bridge_jobs_open's condition (OpenStatesSQL) so SQLite
// can use that partial index instead of scanning every finished job.
func cancelOpenTx(ctx context.Context, tx *sql.Tx, key, now, reason string) error {
	if err := cancelWaitingTx(ctx, tx, key, now, reason); err != nil {
		return err
	}
	// A working job ends at the Bridge's next report; its reason is recorded
	// now and kept through those reports (UpdateHeld), unless an admin asked
	// first.
	_, err := tx.ExecContext(ctx,
		`UPDATE bridge_jobs SET error = CASE WHEN cancel_requested = 1 THEN error ELSE ?3 END,
		        cancel_requested = 1, updated_at = ?2
		  WHERE dedupe_key = ?1 AND state IN `+OpenStatesSQL+` AND state IN `+claimedStatesSQL, key, now, reason)
	return err
}

// cancelWaitingTx ends the open job with dedupe key key at once, inside tx,
// when it waits on no Bridge: queued, or parked (needs_attention). reason is
// its error. A claimed or running one is left as it is.
func cancelWaitingTx(ctx context.Context, tx *sql.Tx, key, now, reason string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE bridge_jobs SET state = 'cancelled', cancel_requested = 1, lease_until = NULL, error = ?3,
		        finished_at = ?2, updated_at = ?2
		  WHERE dedupe_key = ?1 AND state IN `+OpenStatesSQL+` AND state IN ('queued', 'needs_attention')`, key, now, reason)
	return err
}

// CancelWaitingBookJob cancels the job of kind for the book at (libraryID,
// path) inside tx while it is queued or parked (needs_attention), a catalog
// write that leaves it nothing to do (CancelChaptersEdited,
// CancelChaptersChosen). reason is its error. One the Bridge is working on
// (claimed or running) is left to finish: its work is under way, and what it
// writes stays a fallback under the write. No waiting job is no change.
func CancelWaitingBookJob(ctx context.Context, tx *sql.Tx, kind string, libraryID int64, path, reason string, now time.Time) error {
	return cancelWaitingTx(ctx, tx, DedupeKey(kind, &libraryID, &path), at(now), reason)
}

// MoveJobs carries a book's jobs to its new path inside a move's transaction
// (catalog.MoveDurableState), finished ones too (the book's history). An open
// job whose kind already has an open job at the new path is cancelled first,
// since a kind, library and path hold one open job at a time; the rest move,
// their dedupe key rebuilt by DedupeKey. It expires no lease: a job it moves
// keeps its claim and lease as they were, and the next Service call expires a
// lapsed one and settles it once that commits (wakes the claims, reports a job
// failed on its last attempt). Expired here, in the catalog's transaction, a
// failure would never be announced.
func MoveJobs(ctx context.Context, tx *sql.Tx, libraryID int64, oldPath, newPath string, now time.Time) error {
	ts := at(now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE bridge_jobs SET state = 'cancelled', error = ?5,
		        lease_until = NULL, finished_at = ?4, updated_at = ?4
		  WHERE library_id = ?1 AND rel_path = ?2 AND state IN `+OpenStatesSQL+`
		    AND EXISTS (SELECT 1 FROM bridge_jobs n
		                 WHERE n.library_id = ?1 AND n.rel_path = ?3 AND n.kind = bridge_jobs.kind
		                   AND n.state IN `+OpenStatesSQL+`)`,
		libraryID, oldPath, newPath, ts, CancelMoved); err != nil {
		return err
	}
	all, err := jobKinds(ctx, tx, libraryID, oldPath)
	if err != nil {
		return err
	}
	for _, m := range all {
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET rel_path = ?, dedupe_key = ? WHERE id = ?`,
			newPath, DedupeKey(m.kind, &libraryID, &newPath), m.id); err != nil {
			return err
		}
	}
	return nil
}

type jobKind struct {
	id   int64
	kind string
}

// jobKinds lists the ids and kinds of a book's jobs.
func jobKinds(ctx context.Context, tx *sql.Tx, libraryID int64, path string) ([]jobKind, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind FROM bridge_jobs WHERE library_id = ? AND rel_path = ?`, libraryID, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobKind
	for rows.Next() {
		var m jobKind
		if err := rows.Scan(&m.id, &m.kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func requesterID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
