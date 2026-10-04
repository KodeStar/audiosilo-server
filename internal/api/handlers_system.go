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
	"github.com/kodestar/audiosilo-server/internal/web"
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

func installKind() string {
	switch {
	case Version == "dev":
		return installSource
	case fileExists("/.dockerenv") || fileExists("/run/.containerenv"):
		return installDocker
	}
	return installBinary
}

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

// toolVersions caches each tool's version: the paths are fixed for the life of
// the process, and running `-version` takes a moment.
type toolVersions struct {
	mu   sync.Mutex
	seen map[string]string
}

func (tv *toolVersions) get(ctx context.Context, path string) string {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	if v, ok := tv.seen[path]; ok {
		return v
	}
	v, err := toolfetch.Version(ctx, path)
	if err != nil && ctx.Err() != nil {
		return "" // cancelled: ask again next time
	}
	if tv.seen == nil {
		tv.seen = map[string]string{}
	}
	tv.seen[path] = v
	return v
}

func (a *API) tool(ctx context.Context, name, path string) Tool {
	t := Tool{Name: name, Path: path}
	if path == "" {
		return t
	}
	t.Version = a.toolVersions.get(ctx, path)
	t.Source = "local"
	if rel, err := filepath.Rel(filepath.Join(a.boot.DataDir, "tools"), path); err == nil && !strings.HasPrefix(rel, "..") {
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
	wg.Add(3)
	go func() {
		defer wg.Done()
		ffmpeg, ffprobe = a.tool(ctx, "ffmpeg", a.ffmpeg), a.tool(ctx, "ffprobe", a.rt.FFprobe)
	}()
	go func() {
		defer wg.Done()
		if a.metadataOn() {
			h := a.meta.Ping(ctx)
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
		if d, ok := a.scanner.RootDisk(l); ok && ls.Available {
			ls.Disk = &d
		}
		roots = append(roots, ls)
	}

	tlsStatus := TLSStatus{Mode: string(a.boot.TLS.Mode), Hosts: append([]string{}, a.boot.TLS.Hosts...),
		Certificates: []server.Certificate{}}
	if certs, err := server.Certificates(a.boot); err != nil {
		tlsStatus.Error = "the certificate file couldn't be read"
		a.log.Warn("system: read certificate", "err", err)
	} else if certs != nil {
		tlsStatus.Certificates = certs
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":       a.config().DisplayName(),
		"server_id":  a.boot.ServerID,
		"version":    Version,
		"go_version": runtime.Version(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"install":    installKind(),
		"started_at": a.rt.StartedAt.UTC(),
		"data_dir":   a.boot.DataDir,
		"database":   db,
		"tools":      []Tool{ffmpeg, ffprobe},
		"metadata": MetadataStatus{
			Enabled: a.config().Metadata.Enabled, Available: a.meta != nil,
			BaseURL: a.boot.Metadata.BaseURL, Health: metaHealth,
		},
		"tls":        tlsStatus,
		"libraries":  roots,
		"web_player": web.PlayerSource(a.boot.WebDir),
		"update":     a.updateStatus(),
	})
}

// updateStatus is the update check's state, with the install kind the console
// words its "how to update" for. A server without a checker reports it off.
func (a *API) updateStatus() map[string]any {
	st := updates.Status{Current: Version}
	if a.rt.Updates != nil {
		st = a.rt.Updates.Status()
	}
	return map[string]any{
		"enabled": st.Enabled, "current": st.Current, "latest": st.Latest,
		"update_available": st.Available, "comparable": st.Comparable,
		"checked_at": st.CheckedAt, "error": st.Error, "install": installKind(),
	}
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
	if _, err := a.rt.Updates.Check(r.Context()); errors.Is(err, updates.ErrDisabled) {
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
	if a.rt.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": []logring.Entry{}, "last_seq": 0, "truncated": false, "capacity": 0,
		})
		return
	}
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
	limit := min(max(queryInt(r, "limit", 500), 1), maxLogEntries)
	q2 := strings.TrimSpace(q.Get("q"))
	if len(q2) > 200 {
		q2 = q2[:200]
	}
	res := a.rt.Logs.Query(logring.Query{After: after, Level: level, Search: q2, Limit: limit})
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": res.Entries, "last_seq": res.LastSeq, "truncated": res.Truncated,
		"capacity": a.rt.Logs.Capacity(),
	})
}
