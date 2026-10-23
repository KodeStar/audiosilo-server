package api

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast"
	"github.com/kodestar/audiosilo-server/internal/podcast/discovery"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
	"github.com/kodestar/audiosilo-server/internal/podcast/quota"
)

// Podcast discovery and OPML transfer (Bridge Phase 2c-3). Search, its
// providers and its artwork, and the import are the podcasts module's
// listener actions (no kid or demo account, the module on, the old-client
// gate); the export is a core read anyone but a demo account may make, module
// off or Bridge offline. The services are internal/podcast/discovery (search
// and artwork) and internal/podcast (follow by URL, import, export); these
// handlers are transport only.

// OPML transfer budgets.
const (
	opmlImportPerMin  = 2
	opmlImportBurst   = 1
	opmlExportPerMin  = 6
	opmlExportBurst   = 2
	maxOPMLUsers      = 4096
	opmlBucketIdle    = 10 * time.Minute
	maxConcurrentOPML = 2
)

// The OPML export's counts (GET /podcasts/opml/export), named once so the
// handler and the CORS expose list (corsExposeHeaders) cannot drift.
const (
	headerOPMLExported           = "X-OPML-Exported"
	headerOPMLOmittedPrivate     = "X-OPML-Omitted-Private"
	headerOPMLOmittedUnavailable = "X-OPML-Omitted-Unavailable"
)

// opmlTransfers holds the OPML budgets (memory only).
type opmlTransfers struct {
	imports *quota.Limiter
	exports *quota.Limiter
	slots   quota.Slots
}

func newOPMLTransfers() opmlTransfers {
	return opmlTransfers{
		imports: quota.New(opmlImportPerMin, opmlImportBurst, maxOPMLUsers, opmlBucketIdle, nil),
		exports: quota.New(opmlExportPerMin, opmlExportBurst, maxOPMLUsers, opmlBucketIdle, nil),
		slots:   quota.NewSlots(maxConcurrentOPML),
	}
}

// newDiscovery builds the discovery service when podcasts.discovery.enabled was
// on at start (nil otherwise: its routes answer 404 discovery_off). An
// incomplete or malformed Podcast Index pair turns that provider off with one
// warning naming the variable, never its value.
func (a *API) newDiscovery() *discovery.Service {
	d := a.boot.Podcasts.Discovery
	if !d.Enabled {
		return nil
	}
	key, secret, _, problem := d.PodcastIndex()
	if problem != "" {
		a.log.Warn("podcast discovery: Podcast Index search is off", "reason", problem)
	}
	return discovery.New(discovery.Options{
		Version:     Version,
		IndexKey:    key,
		IndexSecret: secret,
		Enabled:     func() bool { return a.moduleOn(bridge.ModulePodcasts) },
		Log:         a.log,
	})
}

// discoveryOn is the podcast_discovery capability: discovery enabled at start
// AND the podcasts module on (whatever the Bridge's state or a provider key).
func (a *API) discoveryOn() bool {
	return a.discovery != nil && a.moduleOn(bridge.ModulePodcasts)
}

// follower is following by URL as the API runs it (podcast.Follower).
func (a *API) follower() *podcast.Follower {
	f := &podcast.Follower{Catalog: a.cat, ModuleOn: func() bool { return a.moduleOn(bridge.ModulePodcasts) }, Log: a.log}
	if a.bridge != nil {
		f.Queue = a.bridge
	}
	return f
}

// mountPodcastTransfer registers the discovery and OPML routes. Every new GET
// also has an explicit HEAD route answering 405 before anything else runs
// (Go's GET pattern would otherwise serve HEAD, fetching to send nothing).
func (a *API) mountPodcastTransfer(mux *http.ServeMux) {
	listener := func(h http.HandlerFunc) http.Handler {
		return a.requireAuth(notKid(notDemo(a.requireModule(bridge.ModulePodcasts, h))))
	}
	for _, p := range []string{"/api/v1/podcasts/discovery/providers", "/api/v1/podcasts/discovery/search", "/api/v1/podcasts/discovery/artwork", "/api/v1/podcasts/opml/export"} {
		mux.HandleFunc("HEAD "+p, headNotAllowed)
	}
	mux.Handle("GET /api/v1/podcasts/discovery/providers", listener(a.handleDiscoveryProviders))
	mux.Handle("GET /api/v1/podcasts/discovery/search", listener(a.handleDiscoverySearch))
	// The artwork is an <img> source: the media equivalent (?token= allowed for
	// a session or API key only, the media rate limit).
	mux.Handle("GET /api/v1/podcasts/discovery/artwork", a.requireMediaAuth(notKid(notDemo(a.requireModule(bridge.ModulePodcasts, http.HandlerFunc(a.handleDiscoveryArtwork))))))
	mux.Handle("POST /api/v1/podcasts/opml/import", listener(a.handleOPMLImport))
	mux.Handle("GET /api/v1/podcasts/opml/export", a.requireAuth(notDemo(http.HandlerFunc(a.handleOPMLExport))))
}

func headNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET")
	noStore(w)
	writeErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET.")
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// writeNoStore writes v as JSON with Cache-Control: no-store.
func writeNoStore(w http.ResponseWriter, status int, v any) {
	noStore(w)
	writeJSON(w, status, v)
}

func writeNoStoreError(w http.ResponseWriter, status int, code, msg string, retryAfter int) {
	noStore(w)
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeErrorCode(w, status, code, msg)
}

func writeDiscoveryError(w http.ResponseWriter, e *discovery.Error) {
	writeNoStoreError(w, e.Status, e.Code, e.Message, e.RetryAfter)
}

// onlyParams refuses a query with a parameter not in allowed, or one given
// twice (400 invalid_request), and reports whether it passed.
func onlyParams(w http.ResponseWriter, q url.Values, allowed ...string) bool {
	for name, vals := range q {
		if len(vals) != 1 || !slices.Contains(allowed, name) {
			writeNoStoreError(w, http.StatusBadRequest, "invalid_request", "Invalid request.", 0)
			return false
		}
	}
	return true
}

// podcastGate answers 403 no_library_access unless the request's client may
// see podcasts (the old-client gate, auth.SeesPodcasts), no admin bypass.
func podcastGate(w http.ResponseWriter, r *http.Request) bool {
	if !callerOf(r.Context()).Podcasts {
		writeNoStoreError(w, http.StatusForbidden, "no_library_access", "No access to podcasts.", 0)
		return false
	}
	return true
}

// discoveryReady is podcastGate, then 404 discovery_off while discovery was
// off at start.
func (a *API) discoveryReady(w http.ResponseWriter, r *http.Request) bool {
	if !podcastGate(w, r) {
		return false
	}
	if a.discovery == nil {
		writeNoStoreError(w, http.StatusNotFound, "discovery_off", "Podcast discovery is turned off.", 0)
		return false
	}
	return true
}

func quotaKey(r *http.Request) string { return strconv.FormatInt(userFrom(r.Context()).ID, 10) }

// handleDiscoveryProviders serves GET /podcasts/discovery/providers.
func (a *API) handleDiscoveryProviders(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.discoveryReady(w, r) || !onlyParams(w, r.URL.Query()) {
		return
	}
	writeNoStore(w, http.StatusOK, map[string]any{"providers": a.discovery.Providers()})
}

// handleDiscoverySearch serves GET /podcasts/discovery/search.
func (a *API) handleDiscoverySearch(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.discoveryReady(w, r) {
		return
	}
	q, e := discovery.ParseQuery(r.URL.Query())
	if e != nil {
		writeDiscoveryError(w, e)
		return
	}
	res, e := a.discovery.Search(r.Context(), quotaKey(r), q)
	if e != nil {
		writeDiscoveryError(w, e)
		return
	}
	writeNoStore(w, http.StatusOK, res)
}

// handleDiscoveryArtwork serves GET /podcasts/discovery/artwork?id=: a
// registered result's artwork as a re-encoded JPEG.
func (a *API) handleDiscoveryArtwork(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.discoveryReady(w, r) || !onlyParams(w, r.URL.Query(), "id", "token") {
		return
	}
	jpg, e := a.discovery.Artwork(r.Context(), quotaKey(r), r.URL.Query().Get("id"))
	if e != nil {
		writeDiscoveryError(w, e)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Content-Length", strconv.Itoa(len(jpg)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(jpg) //nolint:gosec // G705: a JPEG this server re-encoded, sent as image/jpeg with nosniff
}

// opmlTypes are the request types an OPML upload may declare.
var opmlTypes = map[string]bool{"application/xml": true, "text/xml": true, "text/x-opml": true, "application/x-opml+xml": true}

// opmlUploadType reports whether r declares an OPML body: one of opmlTypes,
// no parameter but charset=utf-8, and no content coding but identity.
func opmlUploadType(r *http.Request) bool {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !opmlTypes[strings.ToLower(mt)] {
		return false
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return false
		}
	}
	enc := r.Header.Values("Content-Encoding")
	return len(enc) == 0 || len(enc) == 1 && strings.EqualFold(strings.TrimSpace(enc[0]), "identity")
}

// handleOPMLImport serves POST /podcasts/opml/import?library_id=: the raw
// OPML body, read whole (at most 1 MiB) and validated whole before any row
// acts, then each candidate followed as the caller (podcast.Follower.Import).
func (a *API) handleOPMLImport(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	q := r.URL.Query()
	if !podcastGate(w, r) || !onlyParams(w, q, "library_id") {
		return
	}
	id, err := strconv.ParseInt(q.Get("library_id"), 10, 64)
	if err != nil || id <= 0 {
		writeNoStoreError(w, http.StatusBadRequest, "invalid_request", "A podcast library is required.", 0)
		return
	}
	who := callerOf(r.Context())
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeNoStoreError(w, http.StatusNotFound, "library_not_found", "Library not found.", 0)
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "opml import library failed", "Could not import.")
		return
	}
	// The grant before the kind: a library the caller holds no whole grant on
	// is 403 whatever it holds, so its kind is never revealed.
	whole, err := a.cat.PodcastWholeGrant(r.Context(), who, id)
	if err != nil {
		a.writeCatalogError(w, err, "opml import grant failed", "Could not import.")
		return
	}
	if !whole {
		writeNoStoreError(w, http.StatusForbidden, "no_library_access", "Importing needs access to the whole library.", 0)
		return
	}
	if lib.Kind != catalog.KindPodcasts {
		writeNoStoreError(w, http.StatusConflict, "not_podcast_library", "This library holds books.", 0)
		return
	}
	if !opmlUploadType(r) {
		writeNoStoreError(w, http.StatusUnsupportedMediaType, "unsupported_opml_type", "Send the OPML document itself as XML (UTF-8, uncompressed).", 0)
		return
	}
	if !a.opmlAdmit(w, a.opml.imports.Take(quotaKey(r))) {
		return
	}
	// The body (<= 1 MiB, the person's budget spent) is read before a slot is
	// taken, so a slow upload can't hold one of the two server-wide slots for
	// the whole read timeout.
	body, err := opml.ReadBody(r.Body)
	if err == nil {
		if !a.opml.slots.TryAcquire() {
			writeNoStoreError(w, http.StatusServiceUnavailable, "opml_busy", "OPML transfers are busy. Try again shortly.", 1)
			return
		}
		defer a.opml.slots.Release()
		var doc *opml.Document
		if doc, err = opml.Parse(body); err == nil {
			writeNoStore(w, http.StatusOK, a.follower().Import(r.Context(), who, id, doc))
			return
		}
	}
	if errors.Is(err, opml.ErrLimit) {
		writeNoStoreError(w, http.StatusRequestEntityTooLarge, "opml_limit", "The OPML document is over a size limit.", 0)
		return
	}
	writeNoStoreError(w, http.StatusBadRequest, "invalid_opml", "Invalid OPML document.", 0)
}

// opmlAdmit answers a refused OPML budget: 429 opml_rate_limited with
// Retry-After, or 503 opml_busy while the per-user map is full.
func (a *API) opmlAdmit(w http.ResponseWriter, d quota.Decision) bool {
	switch {
	case d.OK:
		return true
	case d.Full:
		writeNoStoreError(w, http.StatusServiceUnavailable, "opml_busy", "OPML transfers are busy. Try again shortly.", 1)
	default:
		writeNoStoreError(w, http.StatusTooManyRequests, "opml_rate_limited", "Too many OPML transfers. Try again shortly.", d.RetryAfter)
	}
	return false
}

// The export's two 413 opml_export_limit refusals: more subscriptions than
// catalog.MaxPodcastExportRows, or a document past opml.MaxExportBytes.
const (
	msgExportTooMany = "Too many subscriptions to export (more than 10,000)."
	msgExportTooBig  = "The subscription list is too large to export (over 4 MiB)."
)

// handleOPMLExport serves GET /podcasts/opml/export: the caller's followed,
// visible, public subscriptions as an OPML attachment, with the omission
// counts in headers. Private shows are never written.
func (a *API) handleOPMLExport(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !podcastGate(w, r) || !onlyParams(w, r.URL.Query()) {
		return
	}
	if !a.opmlAdmit(w, a.opml.exports.Take(quotaKey(r))) {
		return
	}
	rows, err := a.cat.PodcastExport(r.Context(), callerOf(r.Context()))
	if errors.Is(err, catalog.ErrPodcastExportTooLarge) {
		writeNoStoreError(w, http.StatusRequestEntityTooLarge, "opml_export_limit", msgExportTooMany, 0)
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "opml export failed", "Could not export.")
		return
	}
	exp := podcast.BuildExport(rows)
	out, err := opml.Render(exp.Entries)
	if err != nil {
		writeNoStoreError(w, http.StatusRequestEntityTooLarge, "opml_export_limit", msgExportTooBig, 0)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/x-opml; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="audiosilo-subscriptions.opml"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(headerOPMLExported, strconv.Itoa(exp.Exported))
	h.Set(headerOPMLOmittedPrivate, strconv.Itoa(exp.OmittedPrivate))
	h.Set(headerOPMLOmittedUnavailable, strconv.Itoa(exp.OmittedUnavailable))
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
