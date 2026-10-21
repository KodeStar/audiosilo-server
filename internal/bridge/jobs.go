package bridge

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
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

const (
	// Lease is how long a claim holds a job without a progress update. The
	// Bridge reports at least every 10 s while it works.
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
	// its own (kindSpec.maxParams); MaxParamsBytes is the most any kind takes.
	defaultParamsBytes = 16 << 10
	MaxParamsBytes     = max(defaultParamsBytes, maxCorrectParamsBytes)
	maxProgressBytes   = 16 << 10
	maxResultBytes     = 64 << 10
	maxErrorBytes      = 2 << 10
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
	// is held meanwhile (Service.ModuleOn). A job that needs ModuleContributions
	// files to the community, so it also needs a Bridge that can
	// (ErrCannotContribute).
	module string
	// maxParams bounds the params as sent, in bytes (0: defaultParamsBytes).
	maxParams int
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
// one, or a queued one held while the contributions module is off.
var kinds = map[string]kindSpec{
	KindPodcastFetch:     {needsLibrary: true, book: bookEpisode, writes: true, params: fetchParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindPodcastRetain:    {needsLibrary: true, book: bookShow, writes: true, params: retainParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindPodcastSubscribe: {needsLibrary: true, book: bookNone, writes: true, params: subscribeParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true, suffix: subscribeSuffix},
	KindPodcastPoll:      {needsLibrary: true, book: bookShow, writes: true, params: pollParams, module: ModulePodcasts, libraryKind: libraryPodcasts, internal: true, quiet: true},
	KindVerify:           {needsLibrary: true, book: bookOptional, params: verifyParams},
	KindSidecars: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: sidecarsParams,
		refusedWhileOpen: map[string]error{KindDetectChapters: ErrDetectOpen, KindContribute: ErrContributeOpen},
		cancelsOpen: map[string]string{
			KindDescribe:       "a generate job for the book replaced it",
			KindDetectChapters: "a generate job for the book replaced it; it finds the chapters itself",
			KindContribute:     "a generate job for the book replaced what it was sharing",
		}},
	KindContribute: {libraryKind: libraryBooks, needsLibrary: true, book: bookRequired, params: contributeParams, module: ModuleContributions,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen}},
	KindCorrect: {libraryKind: libraryBooks, needsLibrary: true, book: bookRequired, params: correctParams, module: ModuleContributions,
		maxParams: maxCorrectParamsBytes},
	KindDescribe: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: noParams,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen, KindDetectChapters: ErrDetectOpen}},
	KindDetectChapters: {needsLibrary: true, book: bookRequired, writes: true, libraryKind: libraryBooks, params: noParams,
		refusedWhileOpen: map[string]error{KindSidecars: ErrGenerateOpen, KindDescribe: ErrDescribeOpen}},
}

// claimedStatesSQL are the open states of a job a claim handed to the Bridge
// that it hasn't parked, the ones holding a lease (lapsedSQL: migration 0047's
// idx_bridge_jobs_lease names the same two, in the same order, or expiry
// scans the table).
const claimedStatesSQL = `('claimed', 'running')`

// held reports whether a queued job of kind with params waits for a managed
// module that is off (ModuleFor; with no ModuleOn wired every module counts as
// on): no claim hands it out (nextQueued), and it refuses no new job that
// cancels it (givesWay).
func (s *Service) held(kind string, params json.RawMessage) bool {
	m := ModuleFor(kind, params)
	return m != "" && s.ModuleOn != nil && !s.ModuleOn(m)
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
// claim hands out a queued one meanwhile (nextQueued).
func ModuleFor(kind string, params json.RawMessage) string {
	if kind == KindSidecars && contributes(params) {
		return ModuleContributions
	}
	return kinds[kind].module
}

// noParams checks the params of a kind that takes none (sidecars.describe,
// chapters.detect): only an empty object (or none) passes, any field is
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
	return &j, nil
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

// NewJob is a validated request for a job, after the transport resolved its target.
type NewJob struct {
	Kind      string
	LibraryID *int64
	// Path is the book's indexed path (nil: the job is about the library itself).
	Path         *string
	Params       json.RawMessage
	RequestedBy  int64
	Priority     int
	feedRedacted string
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

// ErrDescribeOpen is a chapters.detect job for a book with a sidecars.describe
// job still open: both rewrite the book's bridge.json on the Bridge, so the
// second is refused (a 409 describe_open).
var ErrDescribeOpen = errors.New("a description job is open for the book")

// ErrChaptersChosen is a chapters.detect job for a book whose chapter source
// an admin chose (the files' or the community's): the chapters it would find
// stand in only when none was chosen, so they would never be used (a 409
// chapters_chosen). The api checks it, as the catalog holds the choice.
var ErrChaptersChosen = errors.New("an admin chose the book's chapter source")

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
// ErrContributeOpen); the open job of its own kind is still returned either way.
// The caller has resolved the library and the book (j.Path is the book's
// indexed path) and checked the job's module is on (ModuleFor).
func (s *Service) Enqueue(ctx context.Context, j NewJob) (job *Job, created bool, err error) {
	if err := checkNew(&j); err != nil {
		return nil, false, err
	}
	if err := s.checkLibraryKind(ctx, j); err != nil {
		return nil, false, err
	}
	h, err := s.LastHeartbeat(ctx)
	if err != nil {
		return nil, false, err
	}
	if h == nil || !h.Offers(j.Kind) {
		return nil, false, ErrKindUnavailable
	}
	var refuse error
	switch {
	case kinds[j.Kind].writes && !h.Writable(*j.LibraryID):
		refuse = ErrLibraryNotWritable
	case ModuleFor(j.Kind, j.Params) == ModuleContributions && !h.CanContribute():
		refuse = ErrCannotContribute
	}
	key := DedupeKey(j.Kind, j.LibraryID, j.Path)
	if suffix := kinds[j.Kind].suffix; suffix != nil {
		key += suffix(j)
	}
	var exp expiry
	identityBusy := false
	err = s.db.WithTx(ctx, "bridge.Enqueue", func(tx *sql.Tx) error {
		now := s.ts()
		var err error
		if exp, err = expireTx(ctx, tx, now); err != nil {
			return err
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
			if !podcastWork(j.Kind) || samePodcastIdentity(job.Params, j.Params) {
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE bridge_jobs SET cancel_requested=1, state=CASE WHEN state IN ('queued','needs_attention') THEN 'cancelled' ELSE state END,finished_at=CASE WHEN state IN ('queued','needs_attention') THEN ? ELSE finished_at END WHERE id=?`, now, id)
			if err != nil {
				return err
			}
			if job.State != StateQueued && job.State != StateNeedsAttention {
				identityBusy = true
				return nil
			}
		}
		if err := checkPodcastTarget(ctx, tx, j); err != nil {
			return err
		}
		if refuse != nil {
			return refuse
		}
		if j.Kind == KindPodcastSubscribe {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs WHERE kind='podcast.subscribe' AND requested_by=? AND state IN `+OpenStatesSQL, j.RequestedBy).Scan(&n); err != nil {
				return err
			}
			if n >= 5 {
				return ErrTooManyRequests
			}
		}
		if podcastWork(j.Kind) {
			if err := s.prunePolls(ctx, tx, j); err != nil {
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
		return nil, false, ErrIdentityChanged
	}
	if created {
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
// code from named, "" for none) when it is news (Notable), on a context the
// caller's leaving doesn't cancel: the move has committed, and the admin should
// hear about it either way.
func (s *Service) report(ctx context.Context, job *Job, from, fromCode string) {
	s.reportEnded(ctx, job, from)
	if s.OnTransition != nil && job.Notable(from, fromCode) {
		s.OnTransition(context.WithoutCancel(ctx), *job, from)
	}
}

// expireDue is expireTx for the read paths: it takes the one writer only when a
// lease has actually run out (a reader-pool check on idx_bridge_jobs_lease
// first), so the console's polls don't queue behind scans for the writer.
func (s *Service) expireDue(ctx context.Context) error {
	now := s.ts()
	var due bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM bridge_jobs WHERE `+lapsedSQL+`)`, now).Scan(&due); err != nil || !due {
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
	return s.waitClaim(ctx, token, ks, wait)
}

// claim hands the Bridge holding token the next queued job of one of kinds
// (highest priority, then oldest) that isn't held (nextQueued), leased for
// Lease, or nil when there is none; ErrNotPaired once token is no longer the
// paired Bridge's.
func (s *Service) claimStored(ctx context.Context, token int64, kinds []string) (*Job, error) {
	var (
		job *Job
		exp expiry
	)
	err := s.db.WithTx(ctx, "bridge.Claim", func(tx *sql.Tx) error {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT `+auth.BridgeTokenLiveSQL, token).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrNotPaired
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

		id, err := s.nextQueued(ctx, tx, kinds)
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
	if err == nil {
		s.settle(ctx, exp) // the other waiting claims wake for the jobs it requeued
	}
	return job, err
}

// nextQueued is the id of the next queued job of one of kinds (highest
// priority, then oldest), 0 for none, passing over a held one: a job that needs
// a managed module that is off (held) waits, queued, until the module is on
// again. The queue is short, so the kinds are matched as read (the statement
// stays a constant).
func (s *Service) nextQueued(ctx context.Context, tx *sql.Tx, kinds []string) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, params FROM bridge_jobs WHERE state = 'queued' ORDER BY priority DESC, id`)
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
		if !s.held(kind, params) {
			return id, rows.Close() // closed before the claim's update: one connection
		}
	}
	return 0, rows.Err()
}

// waitClaim is claim that waits up to wait (at most MaxWait) for a job to be
// queued when there is none, returning nil when none came or ctx ended (the
// caller left, or the server is stopping: a claim cut short by it committed
// nothing).
func (s *Service) waitClaim(ctx context.Context, token int64, kinds []string, wait time.Duration) (*Job, error) {
	wait = min(max(wait, 0), MaxWait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		woken := s.wake.wait()
		job, err := s.claim(ctx, token, kinds)
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
// CancelRequested tells the Bridge an admin asked it to stop. A move into a
// state an admin hears about is reported (OnTransition) once it commits.
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
	err := s.db.WithTx(ctx, "bridge.UpdateHeld", func(tx *sql.Tx) error {
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
			lease = at(s.now().Add(Lease))
		case StateDone, StateFailed, StateCancelled:
			finished = now
		}
		// A field the update leaves out (nil) keeps its value.
		var progress, result, errText any
		if u.Progress != nil {
			progress = string(u.Progress)
		}
		if u.Result != nil {
			result = string(u.Result)
		}
		if u.Error != nil {
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
	return job, nil
}

// transientParks are the park codes that are not news: the agent's rate limit,
// a short outage of the agent container, and a wait for an ASR worker (a
// laptop worker asleep: each book that reaches its transcription would park in
// turn), which the Bridge resumes by itself once they pass (telling an admin
// about every one would flood a phone). Any other park waits on something a
// person sees to, or is long enough to hear about (a monthly budget spent until
// the month turns, a credential refused), whatever its retry_at says.
var transientParks = []string{"agent_rate_limited", "agent_unavailable", "asr_worker_waiting"}

// Notable reports whether a job's move from the state `from` (fromCode: the
// park code it named there, "" for none) is one the server tells admins about
// (notifications): INTO done or failed, or INTO needs_attention with a park
// that isn't transient (transientParks); never a report that leaves the state
// as it was (a running job's keepalive, a parked job's progress). A park that
// changes its code in place is news when the new one isn't transient: the
// Bridge reports a rate limit's retry that ran into the book's budget without a
// running in between, and that park now waits for a person.
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

// ParkCode is the code of the park a job's progress names (progress.park.code,
// sidecars.generate's, cleaned), "" when it names none or can't be read.
func (j *Job) ParkCode() string {
	var p struct {
		Park *struct {
			Code string `json:"code"`
		} `json:"park"`
	}
	if json.Unmarshal(j.Progress, &p) != nil || p.Park == nil {
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

// cancelOpenTx cancels the open job with dedupe key key, inside tx, as Cancel
// does: at once when it is queued or parked, else by asking the Bridge
// (cancel_requested). reason is its error. No open job is no change.
func cancelOpenTx(ctx context.Context, tx *sql.Tx, key, now, reason string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE bridge_jobs SET state = 'cancelled', cancel_requested = 1, lease_until = NULL, error = ?3,
		        finished_at = ?2, updated_at = ?2
		  WHERE dedupe_key = ?1 AND state IN ('queued', 'needs_attention')`, key, now, reason); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE bridge_jobs SET cancel_requested = 1, updated_at = ?2
		  WHERE dedupe_key = ?1 AND state IN `+claimedStatesSQL, key, now)
	return err
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
		`UPDATE bridge_jobs SET state = 'cancelled', error = 'the book moved onto another job of this kind',
		        lease_until = NULL, finished_at = ?4, updated_at = ?4
		  WHERE library_id = ?1 AND rel_path = ?2 AND state IN `+OpenStatesSQL+`
		    AND EXISTS (SELECT 1 FROM bridge_jobs n
		                 WHERE n.library_id = ?1 AND n.rel_path = ?3 AND n.kind = bridge_jobs.kind
		                   AND n.state IN `+OpenStatesSQL+`)`,
		libraryID, oldPath, newPath, ts); err != nil {
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
