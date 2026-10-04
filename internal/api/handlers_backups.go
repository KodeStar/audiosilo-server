package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// Backups (Settings > Backups): the database copied into the backups folder on a
// schedule or on request, listed, downloaded, deleted, and restored at the next
// start. Transport only: internal/backup does the work. A backup holds every
// account's password and token hashes, so downloads and restores are audited like
// any change.

// backupsOff answers when the server has no backup service (never in a real
// server: the launcher always sets one).
func (a *API) backupsOff(w http.ResponseWriter) bool {
	if a.rt.Backups == nil {
		writeError(w, http.StatusNotFound, "backups are not available")
		return true
	}
	return false
}

// backupsEnvelope is GET /admin/backups' answer: the files, the service's state and
// any restore waiting for (or applied at) a start.
func (a *API) backupsEnvelope() (map[string]any, error) {
	list, err := a.rt.Backups.List()
	if err != nil {
		return nil, err
	}
	pending, err := a.rt.Backups.PendingRestore()
	if err != nil {
		return nil, err
	}
	last, err := a.rt.Backups.LastRestore()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"backups": list,
		"status":  a.rt.Backups.Status(),
		"restore": map[string]any{"pending": pending, "last": last},
	}, nil
}

// backupStatus is the backups' state for Health > System (nil without a service).
func (a *API) backupStatus() *backup.Status {
	if a.rt.Backups == nil {
		return nil
	}
	st := a.rt.Backups.Status()
	return &st
}

// handleListBackups lists the backups (admin only): GET /admin/backups.
func (a *API) handleListBackups(w http.ResponseWriter, _ *http.Request) {
	if a.backupsOff(w) {
		return
	}
	env, err := a.backupsEnvelope()
	if err != nil {
		a.writeCatalogError(w, err, "backups: list", "could not list the backups")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// handleCreateBackup starts a backup now (admin only): POST /admin/backups answers
// 202 with the envelope (status.running); 409 backup_running while one is made.
func (a *API) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if a.backupsOff(w) {
		return
	}
	if err := a.rt.Backups.Start(a.baseCtx, backup.KindManual); errors.Is(err, backup.ErrBusy) {
		writeErrorCode(w, http.StatusConflict, codeBackupRunning, "a backup is already being made")
		return
	}
	a.audit(r, "backup.create", "", nil)
	env, err := a.backupsEnvelope()
	if err != nil {
		a.writeCatalogError(w, err, "backups: list", "could not list the backups")
		return
	}
	writeJSON(w, http.StatusAccepted, env)
}

// handleDownloadBackup sends a backup file (admin only): GET /admin/backups/{name}.
func (a *API) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if a.backupsOff(w) {
		return
	}
	name := r.PathValue("name")
	f, b, err := a.rt.Backups.Open(name)
	if err != nil {
		a.writeBackupError(w, err, "backups: open")
		return
	}
	defer func() { _ = f.Close() }()
	a.audit(r, "backup.download", name, nil)
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="`+b.Name+`"`)
	h.Set("Content-Length", strconv.FormatInt(b.Size, 10))
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", b.CreatedAt, f)
}

// handleDeleteBackup removes a backup (admin only): DELETE /admin/backups/{name}.
// A restore waiting for it is cancelled.
func (a *API) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if a.backupsOff(w) {
		return
	}
	name := r.PathValue("name")
	if err := a.rt.Backups.Delete(name); err != nil {
		a.writeBackupError(w, err, "backups: delete")
		return
	}
	a.audit(r, "backup.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreBackup marks a backup to be restored at the next start (admin only):
// POST /admin/backups/{name}/restore. 400 invalid_backup (damaged or not an
// AudioSilo database) or backup_too_new (made by a newer server).
func (a *API) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	if a.backupsOff(w) {
		return
	}
	name := r.PathValue("name")
	u := userFrom(r.Context())
	pending, err := a.rt.Backups.RequestRestore(r.Context(), name, u.Username)
	if err != nil {
		a.writeBackupError(w, err, "backups: request restore")
		return
	}
	a.audit(r, "backup.restore", name, map[string]any{"schema": pending.Schema})
	env, err := a.backupsEnvelope()
	if err != nil {
		a.writeCatalogError(w, err, "backups: list", "could not list the backups")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// handleCancelRestore drops a waiting restore (admin only): DELETE /admin/restore.
func (a *API) handleCancelRestore(w http.ResponseWriter, r *http.Request) {
	if a.backupsOff(w) {
		return
	}
	pending, err := a.rt.Backups.PendingRestore()
	if err != nil {
		a.writeCatalogError(w, err, "backups: read restore", "could not cancel the restore")
		return
	}
	if err := a.rt.Backups.CancelRestore(); err != nil {
		a.writeCatalogError(w, err, "backups: cancel restore", "could not cancel the restore")
		return
	}
	if pending != nil {
		a.audit(r, "backup.restore_cancel", pending.Name, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeBackupError maps the backup service's refusals.
func (a *API) writeBackupError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, backup.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, codeBackupNotFound, "no such backup")
	case errors.Is(err, store.ErrNewerDatabase):
		writeErrorCode(w, http.StatusBadRequest, codeBackupTooNew,
			"this backup was made by a newer AudioSilo server; update this one first")
	case errors.Is(err, store.ErrNotADatabase):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidBackup,
			"this file is damaged or isn't an AudioSilo backup")
	default:
		a.writeCatalogError(w, err, op, "the backup could not be read")
	}
}
