package bridge

import (
	"encoding/json"
	"regexp"
	"slices"
)

// Setup is the Bridge's onboarding checklist (BRIDGE-PLAN.md Phase 2b,
// FIRST-RUN-PLAN.md F2), carried on its heartbeat: what is done and what is
// left before it can run jobs and contribute. The console shows it on
// Settings > Bridge. The Bridge sends it only once a heartbeat reply listed
// "setup" in accepts (HeartbeatAccepts), so an older server, which refuses an
// unknown heartbeat field, never sees it. It is read leniently, like the
// estimates: an unknown key is ignored, a block that doesn't decode is dropped
// and an item the server can't keep is left out, never refusing the heartbeat.
type Setup struct {
	// Items are the checklist's steps in the Bridge's order. An id the server
	// doesn't know is kept: the Bridge owns the list and the console words the
	// ids it knows.
	Items []SetupItem `json:"items"`
	// CanContribute: the Bridge can file contributions now (a GitHub sign-in
	// resolves). It wins over estimates.sidecars.can_contribute
	// (Status.CanContribute).
	CanContribute bool `json:"can_contribute"`
	// GitHubLogin is the GitHub account the Bridge files as ("" when signed
	// out). Admin-only: GET /admin/bridge carries it, GET /server never does.
	GitHubLogin string `json:"github_login,omitempty"`

	// dropped marks a block that didn't decode, which Validate removes (a
	// custom decoder can't set the pointer that holds it to nil).
	dropped bool
}

// SetupItem is one step of the checklist.
type SetupItem struct {
	// ID names the step (^[a-z][a-z0-9_]{0,31}$): paired, libraries, asr, agent,
	// budget, github in Phase 2b.
	ID       string `json:"id"`
	Done     bool   `json:"done"`
	Required bool   `json:"required"`
	// Detail is the Bridge's short note on the step ("1 of 2 libraries
	// writable"), cleaned; "" for none.
	Detail string `json:"detail,omitempty"`
	// LibraryIDs are the server libraries the step is about (the libraries
	// step: the ones still to fix); omitted for none.
	LibraryIDs []int64 `json:"library_ids,omitempty"`
}

// Bounds on a setup block.
const (
	maxSetupItems  = 16
	maxSetupDetail = 256
)

// setupIDPattern is a setup step's id: the same grammar as a module or
// capability id (modules.idPattern's copy; bridge can't import modules).
var setupIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// githubLoginPattern is a GitHub login: letters, digits and single hyphens, at
// most 39 characters, not starting with a hyphen. Anything else is dropped (the
// console shows it).
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// UnmarshalJSON reads a setup block leniently (see Setup): an unknown key is
// ignored, and a block that doesn't decode (a value of the wrong type) is
// marked dropped, for Validate to remove, rather than refusing the heartbeat.
func (s *Setup) UnmarshalJSON(b []byte) error {
	type lenient Setup
	var v lenient
	if err := json.Unmarshal(b, &v); err != nil {
		*s = Setup{dropped: true}
		return nil
	}
	*s = Setup(v)
	return nil
}

// validate is the checklist as kept: nil for none or a block that didn't
// decode, else s bounded in place: at most maxSetupItems items, each with a
// well-formed id listed once (an item that isn't is left out), its detail
// cleaned to printable bounded text, its library ids positive, once each and at
// most maxLibraries; a login that isn't GitHub's shape is dropped.
func (s *Setup) validate() *Setup {
	if s == nil || s.dropped {
		return nil
	}
	items := make([]SetupItem, 0, min(len(s.Items), maxSetupItems))
	for _, it := range s.Items {
		if len(items) == maxSetupItems {
			break
		}
		if !setupIDPattern.MatchString(it.ID) ||
			slices.ContainsFunc(items, func(o SetupItem) bool { return o.ID == it.ID }) {
			continue
		}
		it.Detail = cleanText(it.Detail, maxSetupDetail, false)
		var ids []int64
		for _, id := range it.LibraryIDs {
			if id > 0 && len(ids) < maxLibraries && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		it.LibraryIDs = ids
		items = append(items, it)
	}
	s.Items = items
	if !githubLoginPattern.MatchString(s.GitHubLogin) {
		s.GitHubLogin = ""
	}
	return s
}

// AcceptSetup names the setup block in a heartbeat reply's accepts.
const AcceptSetup = "setup"

// HeartbeatAccepts is the reply's accepts list: the optional heartbeat blocks
// the server reads (PUT /bridge/status). A Bridge sends such a block only after
// a reply named it, as an older server refuses a heartbeat with a field it
// doesn't know.
func HeartbeatAccepts() []string { return []string{AcceptSetup} }
