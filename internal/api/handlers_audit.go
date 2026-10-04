package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The audit log (Server > Audit log). Every admin handler that changes something
// calls audit after the change succeeded, with an action code ("<area>.<verb>"),
// what it was done to, and the facts of the change. Recording is best effort: the
// change has happened, so a failed write is logged, not answered as an error.

// audit records an admin action by the request's caller.
func (a *API) audit(r *http.Request, action, target string, details map[string]any) {
	e := catalog.AuditEvent{Action: action, Target: target, Details: details, Via: catalog.ViaSession}
	if credentialFrom(r.Context()).Kind == auth.KindAPI {
		e.Via = catalog.ViaAPIKey
	}
	if u := userFrom(r.Context()); u != nil {
		id := u.ID
		e.ActorID, e.ActorName = &id, u.Username
	}
	if err := a.cat.RecordAudit(context.WithoutCancel(r.Context()), e); err != nil {
		a.log.Warn("audit: record failed", "action", action, "err", err)
	}
}

// auditAreaRE is what ?area= may be: an action's first part.
var auditAreaRE = regexp.MustCompile(`^[a-z_]{1,32}$`)

// handleAudit lists the audit log, newest first (admin only): GET /admin/audit with
// ?actor_id=, ?area= (user, invite, library, book, share, settings, backup, notify,
// device, progress, issue), ?q= (target or actor, any case), ?before= (the
// previous page's next_before) and ?limit= (<= 200).
func (a *API) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := catalog.AuditFilter{Query: q.Get("q"), Limit: queryInt(r, "limit", 50)}
	actor, ok := parseOptionalID(q.Get("actor_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid actor_id")
		return
	}
	if actor != 0 {
		f.ActorID = &actor
	}
	if area := q.Get("area"); area != "" {
		if !auditAreaRE.MatchString(area) {
			writeError(w, http.StatusBadRequest, "invalid area")
			return
		}
		f.Area = area
	}
	if f.Before, ok = parseOptionalID(q.Get("before")); !ok {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	if len(f.Query) > 200 {
		writeError(w, http.StatusBadRequest, "search too long")
		return
	}
	events, next, err := a.cat.ListAudit(r.Context(), f)
	if err != nil {
		a.writeCatalogError(w, err, "audit: list", "could not load the audit log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_before": next})
}

// Audit targets name things as a person reads them: a username, a share's or a
// library's name, or "#<id>" when the name can't be read (gone, or a failed read).

func orID(name string, err error, id int64) string {
	if err != nil {
		return "#" + strconv.FormatInt(id, 10)
	}
	return name
}

func (a *API) userName(r *http.Request, id int64) string {
	u, err := a.auth.GetUser(r.Context(), id)
	if err != nil {
		return orID("", err, id)
	}
	return u.Username
}

// codeOwner is the username an auth code belongs to.
func (a *API) codeOwner(r *http.Request, id int64) string {
	u, err := a.auth.AuthCodeUser(r.Context(), id)
	if err != nil {
		return orID("", err, id)
	}
	return u.Username
}

func (a *API) shareName(r *http.Request, id int64) string {
	name, err := a.cat.ShareName(r.Context(), id)
	return orID(name, err, id)
}

func (a *API) libraryName(r *http.Request, id int64) string {
	l, err := a.cat.GetLibrary(r.Context(), id)
	if err != nil {
		return orID("", err, id)
	}
	return l.Name
}

// inviteDetails are an invite's limits (never its code).
func inviteDetails(m auth.Minted) map[string]any {
	d := map[string]any{"max_uses": m.MaxUses}
	if m.ExpiresAt != "" {
		d["expires_at"] = m.ExpiresAt
	}
	return d
}

// maxAuditPaths caps the paths an audit event lists (a selection of 1,000 books
// is recorded as its first few and a count).
const maxAuditPaths = 10

// sharePaths are a share change's rules as "<library>: <path>" for the audit log
// (the whole library reads as its name alone).
func sharePaths(a *API, r *http.Request, rules []catalog.PathRule) map[string]any {
	names := map[int64]string{}
	list := []string{}
	for _, rule := range rules {
		if len(list) == maxAuditPaths {
			break
		}
		name, ok := names[rule.LibraryID]
		if !ok {
			name = a.libraryName(r, rule.LibraryID)
			names[rule.LibraryID] = name
		}
		if rule.Path == "" {
			list = append(list, name)
		} else {
			list = append(list, name+": "+rule.Path)
		}
	}
	return map[string]any{"count": len(rules), "first": list}
}
