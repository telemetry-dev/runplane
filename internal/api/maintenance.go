package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/backup"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
)

type Backup struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	S3Key     string    `json:"s3_key" doc:"Object key in the S3 bucket. Empty if not uploaded."`
	Status    string    `json:"status" enum:"running,done,uploading,uploaded,failed" doc:"failed: the backup or its upload to S3 failed, and error tells why. After a failed upload, the file stays on this server."`
	Error     string    `json:"error"`
	Available bool      `json:"available" doc:"The file is on this server, so it can be downloaded."`
}

func (h *Handler) backupOut(b store.Backup) Backup {
	return Backup{
		ID: b.ID, CreatedAt: b.CreatedAt, SizeBytes: b.SizeBytes, S3Key: b.S3Key, Status: b.Status, Error: b.Error,
		Available: h.d.Backups.Available(b),
	}
}

func (h *Handler) listBackups(ctx context.Context, _ *struct{}) (*body[[]Backup], error) {
	list, err := h.d.Store.ListBackups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Backup, len(list))
	for i, b := range list {
		out[i] = h.backupOut(b)
	}
	return &body[[]Backup]{out}, nil
}

func (h *Handler) createBackup(ctx context.Context, _ *struct{}) (*body[Backup], error) {
	id, err := h.d.Backups.Create(ctx)
	if err != nil {
		return nil, serviceErr(err)
	}
	b, err := h.d.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, notFound(err, "The new backup is gone.")
	}
	h.record(ctx, audit.BackupCreate, b.ID, nil)
	return &body[Backup]{h.backupOut(b)}, nil
}

func (h *Handler) downloadBackup(ctx context.Context, in *PathID) (*huma.StreamResponse, error) {
	b, f, err := h.d.Backups.Open(ctx, in.ID)
	switch {
	case errors.Is(err, backup.ErrNoFile):
		return nil, huma.Error404NotFound("The backup file is not on this server.")
	case err != nil:
		return nil, notFound(err, "No backup has this ID.")
	}
	h.record(ctx, audit.BackupDownload, b.ID, nil)
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer f.Close()
		r, w := humago.Unwrap(hctx)
		name := filepath.Base(f.Name())
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeContent(w, r, name, b.CreatedAt, f)
	}}, nil
}

type UpdateStatus struct {
	Current     string     `json:"current" doc:"Version of this runplane."`
	Latest      string     `json:"latest" doc:"Tag of the latest release, for example v0.5.0."`
	Available   bool       `json:"available" doc:"latest is newer than current."`
	URL         string     `json:"url" doc:"Release page."`
	Notes       string     `json:"notes" doc:"Release notes in Markdown."`
	PublishedAt *time.Time `json:"published_at"`
	AutoUpdate  bool       `json:"auto_update" doc:"The auto_update setting."`
	State       string     `json:"state" enum:"idle,applying,restarting" doc:"applying: runplane downloads and installs the update. restarting: the new version starts."`
}

func (h *Handler) getUpdate(ctx context.Context, _ *struct{}) (*body[UpdateStatus], error) {
	st, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	u, err := h.d.Updater.Check(ctx)
	if err != nil {
		return nil, serviceErr(err)
	}
	out := UpdateStatus{
		Current: u.Current, Latest: u.Latest, Available: u.Available, URL: u.URL, Notes: u.Notes,
		AutoUpdate: st.AutoUpdate, State: string(h.d.Updater.State()),
	}
	if !u.PublishedAt.IsZero() {
		out.PublishedAt = &u.PublishedAt
	}
	return &body[UpdateStatus]{out}, nil
}

func (h *Handler) applyUpdate(ctx context.Context, _ *struct{}) (*empty, error) {
	h.record(ctx, audit.UpdateApply, "", nil)
	// The update goes on when the admin closes the page.
	u, err := h.d.Updater.Apply(context.WithoutCancel(ctx))
	switch {
	case errors.Is(err, update.ErrBusy):
		return nil, huma.Error409Conflict("An update is being installed.")
	case errors.Is(err, update.ErrNoUpdate):
		return nil, huma.Error409Conflict("runplane " + u.Current + " is the latest version.")
	case err != nil:
		return nil, serviceErr(err)
	}
	return nil, nil
}

func (h *Handler) installScript(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		r, w := humago.Unwrap(hctx)
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(h.d.InstallScript.Script(r.Host))
	}}, nil
}
