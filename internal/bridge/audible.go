package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/relpath"
)

// Bridge Phase 4a: Audible. The Bridge holds the Audible sign-ins (its vault)
// and polls each account's library; the server stores the accounts' mapping
// and titles (catalog/audible.go) and queues the polls.
const (
	// KindAudiblePoll lists an Audible account's library on the Bridge,
	// matches each title against the account's library on the server and
	// reports the titles (PUT /bridge/audible/accounts/{account}/titles). It
	// writes nothing into the library (an ASIN it backfills goes through the
	// server: PUT /bridge/libraries/{id}/enrichment). Params {account}.
	KindAudiblePoll = "audible.poll"
	// ModuleAudible is the managed module Audible needs (its id:
	// internal/modules/builtin): its admin routes answer 404 module_off and
	// its queued jobs are held while it is off.
	ModuleAudible = "audible"
	// FeatureAudible is the feature of a Bridge that runs Audible jobs (the
	// family of KindAudiblePoll), the module's Bridge requirement.
	FeatureAudible = "audible"
)

// AudibleAccountPattern is an Audible account's id: the Bridge makes it,
// opaque to the server.
var AudibleAccountPattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// AudiblePollParams are audible.poll's params: the account to poll and its
// placement template at queue time ("" = the automatic placement, omitted),
// which the Bridge plans each unmatched title's backup by.
type AudiblePollParams struct {
	Account  string `json:"account"`
	Template string `json:"template,omitempty"`
}

// audiblePollParams checks audible.poll's params strictly.
func audiblePollParams(raw json.RawMessage) (json.RawMessage, error) {
	var p AudiblePollParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if !AudibleAccountPattern.MatchString(p.Account) {
		return nil, invalid("account must match %s", AudibleAccountPattern)
	}
	// Bounded, not parsed: a template the grammar later refuses must never
	// stop the account's polls (the Bridge reports such a plan as an error).
	if !AudibleTemplateBounded(p.Template) {
		return nil, invalid("template must be at most %d bytes", MaxAudibleTemplate)
	}
	return json.Marshal(p)
}

// audibleAccountSuffix extends an audible.poll's or audible.sync's dedupe key
// with its params' account, so each account has at most one open job of the
// kind.
func audibleAccountSuffix(j NewJob) string {
	var p struct {
		Account string `json:"account"`
	}
	_ = json.Unmarshal(j.Params, &p)
	return "|" + p.Account
}

// AudiblePoll is the NewJob that polls account, planning its unmatched
// titles by template (the account's).
func AudiblePoll(account, template string, priority int) NewJob {
	raw, _ := json.Marshal(AudiblePollParams{Account: account, Template: template})
	return NewJob{Kind: KindAudiblePoll, Params: raw, Priority: priority}
}

// pruneAudibleEnded keeps only the newest ended poll (or sync, kind) of the
// account a new one is queued for (its dedupe key), inside Enqueue's
// transaction: a poll every few hours, or a sync every 15 minutes, would
// otherwise pile up in the job list.
func pruneAudibleEnded(ctx context.Context, tx *sql.Tx, kind, key string) error {
	// The kind lets idx_bridge_jobs_kind_finished serve both scans (the
	// dedupe_key index holds only open jobs).
	_, err := tx.ExecContext(ctx, `DELETE FROM bridge_jobs WHERE kind = ?2 AND dedupe_key = ?1 AND state IN `+EndedStatesSQL+`
		AND id <> (SELECT MAX(id) FROM bridge_jobs WHERE kind = ?2 AND dedupe_key = ?1 AND state IN `+EndedStatesSQL+`)`,
		key, kind)
	return err
}

// The server's reasons for cancelling an account's open poll (IsCancelReason;
// the console keeps a copy: admin-ui generate-model.ts).
const (
	// CancelAudibleRemoved: the account was removed (by the Bridge, or the console).
	CancelAudibleRemoved = "the Audible account was removed"
	// CancelAudiblePaused: the account's library was deleted (it is paused).
	CancelAudiblePaused = "the Audible account's library was deleted"
	// CancelAudibleSyncOff: the console turned the account's sync off (an open
	// audible.sync, Phase 4c).
	CancelAudibleSyncOff = "sync with Audible was turned off for the account"
)

// CancelAudiblePolls cancels account's open poll inside tx, reason one of
// CancelAudibleRemoved or CancelAudiblePaused: at once when queued or parked,
// else at the Bridge's next report.
func CancelAudiblePolls(ctx context.Context, tx *sql.Tx, account, reason string, now time.Time) error {
	return cancelOpenTx(ctx, tx, jobKey(AudiblePoll(account, "", 0)), at(now), reason)
}

// CancelAudibleImports cancels every open backup of account inside tx (the
// account was removed: CancelAudibleRemoved), as CancelAudiblePolls does its
// poll: at once when queued or parked, else at the Bridge's next report.
func CancelAudibleImports(ctx context.Context, tx *sql.Tx, account, reason string, now time.Time) error {
	keys, err := queryColumn[string](ctx, tx, `SELECT dedupe_key FROM bridge_jobs WHERE kind = ? AND state IN `+OpenStatesSQL+`
		AND json_extract(params, '$.account') = ?`, KindAudibleImport, account)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := cancelOpenTx(ctx, tx, key, at(now), reason); err != nil {
			return err
		}
	}
	return nil
}

// CleanText is v as printable bounded text on one line (cleanText): what the
// Bridge reports from the outside world (an Audible title, its authors) is
// cleaned rather than refused, so one odd title never fails a whole poll.
func CleanText(v string, max int) string { return strings.TrimSpace(cleanText(v, max, false)) }

// Bridge Phase 4b: backing up an Audible title.

// KindAudibleImport backs up one title an account owns into the account's
// library: the Bridge downloads and decrypts it, places it beside the series'
// other books (or by the account's template, or at the console's destination),
// rescans it and sets its ASIN. Params AudibleImportParams; the server queues
// it (internal), one open per account and title (audibleImportSuffix).
const KindAudibleImport = "audible.import"

// AudibleImportParams are audible.import's params, built by the server when it
// queues the job: the account and title, the account's library (the job's
// library_id too), and the console's destination and the account's template
// at queue time ("" for none). In both, a last segment without an audio
// extension is a folder the Bridge puts the title's file in. The Bridge reads
// them leniently (unknown fields ignored, the ASIN upper-cased).
type AudibleImportParams struct {
	Account   string `json:"account"`
	ASIN      string `json:"asin"`
	LibraryID int64  `json:"library_id"`
	DestPath  string `json:"dest_path"`
	Template  string `json:"template"`
}

// The audible.import result's and failure's codes the console names (the
// Bridge's; a failed job's code is AudibleImportCode's reading).
const (
	ImportNotReleased       = "not_released"
	ImportNoRights          = "no_rights"
	ImportAudibleRefused    = "audible_refused"
	ImportNotWritable       = "not_writable"
	ImportExists            = "exists"
	ImportDiskFull          = "disk_full"
	ImportOfflineURLExpired = "offline_url_expired"
	ImportUnknownAccount    = "unknown_account"
)

// AudibleImportCodes are the codes an audible.import failure may name.
var AudibleImportCodes = []string{ImportNotReleased, ImportNoRights, ImportAudibleRefused, ImportNotWritable,
	ImportExists, ImportDiskFull, ImportOfflineURLExpired, ImportUnknownAccount}

var asinPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)

const (
	// MaxAudibleDestPath and MaxAudibleTemplate bound a destination and a
	// template, in bytes.
	MaxAudibleDestPath = 1024
	MaxAudibleTemplate = 500
)

// AudibleTemplateBounded reports whether s is a template within its bounds
// (MaxAudibleTemplate bytes of UTF-8), not whether it parses: what a poll's
// params and its planned destinations carry.
func AudibleTemplateBounded(s string) bool {
	return len(s) <= MaxAudibleTemplate && utf8.ValidString(s)
}

// ErrAudibleDestPath and ErrAudibleTemplate are a destination or template the
// server refuses (400 invalid_dest_path, invalid_template).
var (
	ErrAudibleDestPath = errors.New("invalid destination")
	ErrAudibleTemplate = errors.New("invalid template")
)

// CheckAudibleDestPath checks a console destination for a backup: the
// library-relative path of the file to place (folders and file name, the
// manager's Dests) or of the folder it goes in (a last segment without an
// audio extension: the Bridge adds the title's file name), "" for none. It must be clean as relpath.Clean leaves it
// (no "..", no leading or trailing slash, no "." segment), name no hidden
// segment (the scan skips them: library.ContentPath's rule), and hold no
// backslash or control character. The Bridge checks it again against its
// mapping (SafeJoin) when it places the file.
func CheckAudibleDestPath(p string) error {
	if p == "" {
		return nil
	}
	switch {
	case len(p) > MaxAudibleDestPath:
		return fmt.Errorf("%w: at most %d bytes", ErrAudibleDestPath, MaxAudibleDestPath)
	case strings.ContainsRune(p, '\\') || strings.ContainsFunc(p, isControl) || !utf8.ValidString(p):
		return fmt.Errorf("%w: no backslashes or control characters", ErrAudibleDestPath)
	case relpath.Clean(p) != p:
		return fmt.Errorf("%w: a clean path inside the library, like Author/Series/Title.m4b", ErrAudibleDestPath)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("%w: no hidden folders or files", ErrAudibleDestPath)
		}
	}
	return nil
}

// audibleTemplateVars are the variables a placement template may name (the
// Bridge's placement.knownVars; a test there holds them together).
var audibleTemplateVars = []string{"Author", "Series", "Title", "TitleShort", "Seq", "PaddedSeq", "SHORTCODE",
	"ASIN", "Narrator", "Year", "OriginalFilename", "Ext"}

// CheckAudibleTemplate checks an account's placement template ("" for the
// Bridge's automatic placement) by the grammar of the Bridge's
// placement.Parse: literal text, {Var} or {Var:mods} tokens (mods width=N,
// upper, lower, trim), optional groups { ... }, and \ escaping a character.
// It must also be bounded, printable, relative (no leading slash) and name no
// ".." or hidden segment (one starting with a dot: the scan skips it, as
// CheckAudibleDestPath). The Bridge parses it again when it places a backup.
func CheckAudibleTemplate(s string) error {
	if s == "" {
		return nil
	}
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrAudibleTemplate, why) }
	switch {
	case len(s) > MaxAudibleTemplate:
		return bad(fmt.Sprintf("at most %d bytes", MaxAudibleTemplate))
	case strings.ContainsFunc(s, isControl) || !utf8.ValidString(s):
		return bad("no control characters")
	case strings.HasPrefix(strings.TrimSpace(s), "/"):
		return bad("a path inside the library, not starting with /")
	}
	for seg := range strings.SplitSeq(s, "/") {
		switch seg = strings.TrimPrefix(strings.TrimSpace(seg), `\`); {
		case seg == "..":
			return bad(`no ".." folders`)
		case strings.HasPrefix(seg, "."):
			return bad("no hidden folders or files (a part starting with a dot)")
		}
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			i++ // escapes the next byte (a trailing one is literal)
		case '}':
			if depth == 0 {
				return bad(fmt.Sprintf("unexpected } at position %d", i))
			}
			depth--
		case '{':
			name, end, isVar := templateVar(s, i)
			if !isVar {
				depth++
				continue
			}
			if !slices.Contains(audibleTemplateVars, name) {
				return bad(fmt.Sprintf("unknown variable {%s}", name))
			}
			if s[end] == ':' {
				close := strings.IndexAny(s[end+1:], "{}")
				if close < 0 || s[end+1+close] != '}' {
					return bad(fmt.Sprintf("malformed modifier in {%s:...}", name))
				}
				if err := templateMods(s[end+1 : end+1+close]); err != nil {
					return bad(fmt.Sprintf("{%s}: %v", name, err))
				}
				end += 1 + close
			}
			i = end
		}
	}
	if depth != 0 {
		return bad("unclosed { (optional group)")
	}
	return nil
}

// templateVar reads a variable token at s[i] ('{'): a letter-led name of
// letters and digits followed by '}' or ':'. end is the index of that '}' or
// ':'; isVar false means the '{' opens an optional group.
func templateVar(s string, i int) (name string, end int, isVar bool) {
	j := i + 1
	if j >= len(s) || !isASCIILetter(s[j]) {
		return "", 0, false
	}
	for j < len(s) && (isASCIILetter(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j >= len(s) || (s[j] != '}' && s[j] != ':') {
		return "", 0, false
	}
	return s[i+1 : j], j, true
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// templateMods checks a variable's modifiers: width=N (N >= 0), upper, lower,
// trim, comma-separated (an empty part skipped, as the Bridge's parseMods; an
// empty name with a value, "=x", is unknown there too).
func templateMods(spec string) error {
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		switch key {
		case "width":
			if n, err := strconv.Atoi(val); !hasVal || err != nil || n < 0 {
				return fmt.Errorf("invalid width %q", val)
			}
		case "upper", "lower", "trim":
		default:
			return fmt.Errorf("unknown modifier %q", key)
		}
	}
	return nil
}

// audibleImportParams checks audible.import's params strictly.
func audibleImportParams(raw json.RawMessage) (json.RawMessage, error) {
	var p AudibleImportParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	switch {
	case !AudibleAccountPattern.MatchString(p.Account):
		return nil, invalid("account must match %s", AudibleAccountPattern)
	case !asinPattern.MatchString(p.ASIN):
		return nil, invalid("asin must be an ASIN")
	case p.LibraryID <= 0:
		return nil, invalid("library_id is required")
	}
	if err := CheckAudibleDestPath(p.DestPath); err != nil {
		return nil, invalid("dest_path: %v", err)
	}
	if err := CheckAudibleTemplate(p.Template); err != nil {
		return nil, invalid("template: %v", err)
	}
	return json.Marshal(p)
}

// audibleImportSuffix extends an audible.import's dedupe key with its account
// and title, so each title has at most one open import.
func audibleImportSuffix(j NewJob) string {
	var p AudibleImportParams
	_ = json.Unmarshal(j.Params, &p)
	return "|" + p.Account + "|" + p.ASIN
}

// AudibleImport is the NewJob that backs up p's title into p's library.
func AudibleImport(p AudibleImportParams, requestedBy int64, priority int) NewJob {
	raw, _ := json.Marshal(p)
	lib := p.LibraryID
	return NewJob{Kind: KindAudibleImport, LibraryID: &lib, Params: raw, RequestedBy: requestedBy, Priority: priority}
}

// AudibleImportResult is a done audible.import's result: where the book was
// placed (its indexed path, as the Bridge's rescan answered it) and what was
// written.
type AudibleImportResult struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Files     []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"files"`
	Bytes    int64 `json:"bytes"`
	Enriched bool  `json:"enriched"`
}

// AudibleImportOf reads a job's params and, when it is done, its result.
func AudibleImportOf(j Job) (p AudibleImportParams, r AudibleImportResult, err error) {
	if err = json.Unmarshal(j.Params, &p); err != nil {
		return p, r, err
	}
	if j.State == StateDone && len(j.Result) > 0 {
		err = json.Unmarshal(j.Result, &r)
	}
	return p, r, err
}

// AudibleImportCode is the code an ended or parked audible.import names, ""
// for none: a park's code (needs_attention), else the result's "code" (the
// Bridge reports a failure as {"code","message"}), else an error that starts
// with one of AudibleImportCodes ("exists: ..."). Cleaned and bounded.
func AudibleImportCode(j Job) string {
	switch j.State {
	case StateNeedsAttention:
		return j.ParkCode()
	case StateFailed, StateCancelled:
	default:
		return ""
	}
	return failureCode(j, AudibleImportCodes)
}

// failureCode is the code an ended audible job failed with, "" for none: the
// result's "code" (the Bridge reports a failure as {"code","message"}),
// cleaned and bounded, else one of codes its error starts with ("exists: ...").
func failureCode(j Job, codes []string) string {
	var r struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(j.Result, &r) == nil && r.Code != "" {
		return cleanText(r.Code, maxShortText, false)
	}
	for _, c := range codes {
		if j.Error == c || strings.HasPrefix(j.Error, c+":") {
			return c
		}
	}
	return ""
}

// Bridge Phase 4c: keeping an account's places in step with Audible.

// KindAudibleSync reads an account's places on Audible (lastpositions) for the
// titles its params name and posts the ones ahead of this server's for the
// account's person (POST /bridge/audible/accounts/{account}/listening,
// furthest only); the other direction is the Bridge's push loop over GET
// /bridge/progress, not a job. Params AudibleSyncParams; the server queues it
// (internal, quiet) every AudibleSyncEvery per synced account, one open per
// account (audibleAccountSuffix).
const KindAudibleSync = "audible.sync"

// AudibleSyncEvery is how often the scheduler queues a synced account's
// audible.sync.
const AudibleSyncEvery = 15 * time.Minute

// The audible.sync failure code the server reads (AudibleSyncCode): Audible
// refused the account (the Bridge's "Sign in again" state), so no sync is
// queued until the Bridge maps the account again.
const SyncAudibleRefused = ImportAudibleRefused

// AudibleSyncTitle is one title an audible.sync reads: its ASIN, the book on
// this server it is matched to (library and book path, the progress key the
// Bridge's events name) and Audible's runtime in seconds (0 unknown), which
// the Bridge's runtime guards read.
type AudibleSyncTitle struct {
	ASIN      string `json:"asin"`
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	RuntimeS  int64  `json:"runtime_s"`
}

// AudibleSyncParams are audible.sync's params, built by the server when it
// queues the job: the account and every title to sync (matched to a book on
// this server, not marked "Not this book" or "Don't sync this book", not gone
// from the account), by ASIN.
type AudibleSyncParams struct {
	Account string             `json:"account"`
	Titles  []AudibleSyncTitle `json:"titles"`
}

const (
	// maxAudibleSyncParamsBytes bounds audible.sync's params: thousands of
	// titles (about 120 bytes each); AudibleSync leaves out the last titles of
	// a list that would be longer.
	maxAudibleSyncParamsBytes = 1 << 20
	// maxSyncPathBytes is the longest book path a title may name (the
	// catalog's MaxPathBytes).
	maxSyncPathBytes = 4096
)

// audibleSyncParams checks audible.sync's params strictly: an account id, and
// titles each with an ASIN, a library and a clean book path (once each ASIN).
func audibleSyncParams(raw json.RawMessage) (json.RawMessage, error) {
	var p AudibleSyncParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if !AudibleAccountPattern.MatchString(p.Account) {
		return nil, invalid("account must match %s", AudibleAccountPattern)
	}
	if p.Titles == nil {
		p.Titles = []AudibleSyncTitle{}
	}
	seen := make(map[string]bool, len(p.Titles))
	for _, t := range p.Titles {
		switch {
		case !asinPattern.MatchString(t.ASIN) || seen[t.ASIN]:
			return nil, invalid("titles: each asin must be an ASIN, once")
		case t.LibraryID <= 0:
			return nil, invalid("titles: library_id is required")
		case t.Path == "" || len(t.Path) > maxSyncPathBytes || (t.Path != "." && relpath.Clean(t.Path) != t.Path):
			return nil, invalid("titles: path must be a clean book path")
		case t.RuntimeS < 0:
			return nil, invalid("titles: runtime_s must not be negative")
		}
		seen[t.ASIN] = true
	}
	return json.Marshal(p)
}

// AudibleSync is the NewJob that syncs account's titles (in the order given),
// leaving out the last ones when their params would pass
// maxAudibleSyncParamsBytes (dropped reports how many).
func AudibleSync(account string, titles []AudibleSyncTitle, priority int) (job NewJob, dropped int) {
	if titles == nil {
		titles = []AudibleSyncTitle{}
	}
	keep := titles
	raw, _ := json.Marshal(AudibleSyncParams{Account: account, Titles: keep})
	for len(raw) > maxAudibleSyncParamsBytes && len(keep) > 0 {
		keep = keep[:len(keep)*9/10]
		raw, _ = json.Marshal(AudibleSyncParams{Account: account, Titles: keep})
	}
	return NewJob{Kind: KindAudibleSync, Params: raw, Priority: priority}, len(titles) - len(keep)
}

// AudibleSyncOf reads an audible.sync's account.
func AudibleSyncOf(j Job) (account string, err error) {
	var p AudibleSyncParams
	err = json.Unmarshal(j.Params, &p)
	return p.Account, err
}

// AudibleSyncCode is the code an ended audible.sync failed with, "" for none:
// the result's "code", else an error starting with SyncAudibleRefused.
func AudibleSyncCode(j Job) string {
	if j.State != StateFailed {
		return ""
	}
	return failureCode(j, []string{SyncAudibleRefused})
}

// CancelAudibleSyncs cancels account's open sync inside tx (removed or paused,
// as CancelAudiblePolls).
func CancelAudibleSyncs(ctx context.Context, tx *sql.Tx, account, reason string, now time.Time) error {
	job, _ := AudibleSync(account, nil, 0)
	return cancelOpenTx(ctx, tx, jobKey(job), at(now), reason)
}
