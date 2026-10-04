package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/logring"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/server"
	"github.com/kodestar/audiosilo-server/internal/toolfetch"
	"github.com/kodestar/audiosilo-server/internal/updates"
)

// Runtime is what the launcher knows about the running server that the admin
// console reports on Health > System, Server > About and Server > Logs.
type Runtime struct {
	FFprobe   string           // the ffprobe in use ("" when off)
	StartedAt time.Time        // when the server started (New sets it; the launcher may too)
	Logs      *logring.Ring    // the log viewer's records (nil: no viewer)
	Updates   *updates.Checker // the update check (nil: none)
}

// SetRuntime sets what the launcher reports about the process. Call before Handler().
func (a *API) SetRuntime(rt Runtime) {
	if rt.StartedAt.IsZero() {
		rt.StartedAt = a.rt.StartedAt
	}
	a.rt = rt
}

// Install kinds, for how the console says to update.
const (
	installDocker = "docker" // running in a container
	installBinary = "binary" // a release binary
	installSource = "source" // a local build (version "dev")
)

// installKind is how this server was installed; it can't change while it runs.
var installKind = sync.OnceValue(func() string {
	switch {
	case Version == "dev":
		return installSource
	case fileExists("/.dockerenv") || fileExists("/run/.containerenv"):
		return installDocker
	}
	return installBinary
})

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Tool is ffmpeg or ffprobe as Health > System shows it.
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`    // "" when off or not found
	Version string `json:"version"` // from `-version`, "" if it didn't say
	// Source: "local" (found next to the binary or on PATH, or configured),
	// "downloaded" (fetched into <data>/tools), or "" when there is none.
	Source string `json:"source"`
}

// toolVersions runs each tool's `-version` once (the paths are fixed for the
// life of the process), on its own deadline rather than a request's, so a
// cancelled request neither caches a blank nor makes the next one wait again.
type toolVersions struct {
	versions sync.Map // path -> func() string (sync.OnceValue)
}

func (tv *toolVersions) get(path string) string {
	fn, _ := tv.versions.LoadOrStore(path, sync.OnceValue(func() string {
		v, _ := toolfetch.Version(context.Background(), path)
		return v
	}))
	return fn.(func() string)()
}

func (a *API) tool(name, path string) Tool {
	t := Tool{Name: name, Path: path}
	if path == "" {
		return t
	}
	t.Version = a.toolVersions.get(path)
	t.Source = "local"
	if rel, err := filepath.Rel(toolfetch.Dir(a.config().DataDir), path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Source = "downloaded"
	}
	return t
}

// MetadataStatus is the community metadata service on Health > System.
type MetadataStatus struct {
	Enabled   bool         `json:"enabled"`
	Available bool         `json:"available"` // the service exists (base_url valid at start)
	BaseURL   string       `json:"base_url"`
	Health    *meta.Health `json:"health"` // nil while off: nothing is asked
}

// LibraryStatus is a library root on Health > System.
type LibraryStatus struct {
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	Root      string        `json:"root"`
	Available bool          `json:"available"`
	Disk      *library.Disk `json:"disk"` // nil when the root doesn't answer
}

// TLSStatus is the server's TLS on Health > System.
type TLSStatus struct {
	Mode         string               `json:"mode"`
	Hosts        []string             `json:"hosts"`
	Certificates []server.Certificate `json:"certificates"`
	Error        string               `json:"error,omitempty"` // a certificate file that couldn't be read
}

// handleSystem reports everything the server depends on (admin only):
// GET /admin/system. Nothing here reaches outside the server except the
// metadata health check, and that only while the lookup is on.
func (a *API) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db, err := a.cat.DatabaseInfo(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "system: database info", "could not read the system status")
		return
	}
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "system: list libraries", "could not read the system status")
		return
	}
	counts, err := a.cat.CountBooksByLibrary(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "system: count books", "could not read the system status")
		return
	}

	// The slow parts (a tool's first -version, the metadata ping, root probes)
	// run side by side; each is bounded on its own.
	var (
		wg              sync.WaitGroup
		ffmpeg, ffprobe Tool
		metaHealth      *meta.Health
		avail           map[int64]bool
	)
	wg.Add(4)
	go func() { defer wg.Done(); ffmpeg = a.tool("ffmpeg", a.ffmpeg) }()
	go func() { defer wg.Done(); ffprobe = a.tool("ffprobe", a.rt.FFprobe) }()
	go func() {
		defer wg.Done()
		if a.metadataOn() {
			h := a.meta.Ping()
			metaHealth = &h
		}
	}()
	go func() {
		defer wg.Done()
		avail = a.scanner.RootsAvailable(libs, counts)
	}()
	wg.Wait()

	roots := make([]LibraryStatus, 0, len(libs))
	for _, l := range libs {
		ls := LibraryStatus{ID: l.ID, Name: l.Name, Root: l.Root, Available: avail[l.ID]}
		if ls.Available {
			if d, ok := a.scanner.RootDisk(l); ok {
				ls.Disk = &d
			}
		}
		roots = append(roots, ls)
	}

	tlsStatus := TLSStatus{Mode: string(a.config().TLS.Mode), Hosts: append([]string{}, a.config().TLS.Hosts...),
		Certificates: []server.Certificate{}}
	if certs, err := server.Certificates(a.config().Config); err != nil {
		tlsStatus.Error = "the certificate file couldn't be read"
		a.log.Warn("system: read certificate", "err", err)
	} else if certs != nil {
		tlsStatus.Certificates = certs
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":       a.config().DisplayName(),
		"server_id":  a.config().ServerID,
		"version":    Version,
		"go_version": runtime.Version(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"install":    installKind(),
		"started_at": a.rt.StartedAt.UTC(),
		"data_dir":   a.config().DataDir,
		"database":   db,
		"tools":      []Tool{ffmpeg, ffprobe},
		"metadata": MetadataStatus{
			Enabled: a.config().Metadata.Enabled, Available: a.meta != nil,
			BaseURL: a.config().Metadata.BaseURL, Health: metaHealth,
		},
		"tls":        tlsStatus,
		"libraries":  roots,
		"web_player": a.playerSource,
		"update":     a.updateStatus(),
	})
}

// UpdateStatus is the update check's state, with the install kind the console
// words its "how to update" for.
type UpdateStatus struct {
	updates.Status
	Install string `json:"install"`
}

// updateStatus reports the update check; a server without a checker reports it off.
func (a *API) updateStatus() UpdateStatus {
	st := updates.Status{Current: Version}
	if a.rt.Updates != nil {
		st = a.rt.Updates.Status()
	}
	return UpdateStatus{Status: st, Install: installKind()}
}

// handleUpdateStatus reports the update check (admin only): GET /admin/update.
func (a *API) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.updateStatus())
}

// handleUpdateCheck asks GitHub now (admin only): POST /admin/update/check. 409
// update_check_off while the check is off; within a minute of the last request
// it answers with that request's result.
func (a *API) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if a.rt.Updates == nil {
		writeErrorCode(w, http.StatusConflict, codeUpdateCheckOff, "the update check is turned off")
		return
	}
	if err := a.rt.Updates.Check(r.Context()); errors.Is(err, updates.ErrDisabled) {
		writeErrorCode(w, http.StatusConflict, codeUpdateCheckOff, "the update check is turned off")
		return
	}
	writeJSON(w, http.StatusOK, a.updateStatus())
}

// maxLogEntries caps one GET /admin/logs answer.
const maxLogEntries = 1000

// handleLogs returns the newest log lines (admin only): GET /admin/logs with
// ?level=info|warn|error (at least), ?q= (contains, any case), ?after=<seq> (the
// live tail's cursor: only newer lines) and ?limit= (<= 1000, the newest).
func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	level, ok := logring.ParseLevel(q.Get("level"))
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be info, warn or error")
		return
	}
	var after uint64
	if s := q.Get("after"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid after")
			return
		}
		after = n
	}
	search := q.Get("q")
	if len(search) > 200 {
		search = search[:200]
	}
	res := logring.Result{Entries: []logring.Entry{}}
	if a.rt.Logs != nil {
		res = a.rt.Logs.Query(logring.Query{
			After: after, Level: level, Search: search,
			Limit: min(max(queryInt(r, "limit", 500), 1), maxLogEntries),
		})
	}
	writeJSON(w, http.StatusOK, res)
}
