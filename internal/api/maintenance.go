package api

import (
	"cmp"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

type Backup struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	S3Key     string    `json:"s3_key" doc:"Object key in the S3 bucket. Empty if not uploaded."`
	Status    string    `json:"status" enum:"running,done,uploading,uploaded,failed"`
	Error     string    `json:"error"`
	Available bool      `json:"available" doc:"The file is on this server, so it can be downloaded."`
}

func (h *Handler) backupOut(b store.Backup) Backup {
	_, err := os.Stat(b.Path)
	return Backup{
		ID: b.ID, CreatedAt: b.CreatedAt, SizeBytes: b.SizeBytes, S3Key: b.S3Key, Status: b.Status, Error: b.Error,
		Available: err == nil && h.inBackupsDir(b.Path),
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
	if h.d.Backups == nil {
		return nil, unavailable("Backups")
	}
	id, err := h.d.Backups.Run(ctx)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	b, err := h.d.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, notFound(err, "The new backup is gone.")
	}
	h.audit(ctx, "backup.create", b.ID, nil)
	h.d.Bus.Publish(events.Event{Type: events.BackupState, Data: b})
	return &body[Backup]{h.backupOut(b)}, nil
}

func (h *Handler) downloadBackup(ctx context.Context, in *PathID) (*huma.StreamResponse, error) {
	b, err := h.d.Store.GetBackup(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No backup has this ID.")
	}
	if !h.inBackupsDir(b.Path) {
		return nil, huma.Error404NotFound("The backup file is not on this server.")
	}
	f, err := os.Open(b.Path)
	if os.IsNotExist(err) {
		return nil, huma.Error404NotFound("The backup file is not on this server.")
	}
	if err != nil {
		return nil, err
	}
	h.audit(ctx, "backup.download", b.ID, nil)
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer f.Close()
		r, w := humago.Unwrap(hctx)
		name := filepath.Base(b.Path)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeContent(w, r, name, b.CreatedAt, f)
	}}, nil
}

// inBackupsDir guards the download against a path outside the backups folder.
func (h *Handler) inBackupsDir(path string) bool {
	rel, err := filepath.Rel(h.d.Config.BackupsDir(), path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
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
	if h.d.Updater == nil {
		return nil, unavailable("The updater")
	}
	st, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	u, err := h.d.Updater.Check(ctx)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	u.AutoUpdate = st.AutoUpdate
	h.updateMu.Lock()
	u.State = cmp.Or(h.updating, u.State, "idle")
	h.updateMu.Unlock()
	return &body[UpdateStatus]{u}, nil
}

func (h *Handler) applyUpdate(ctx context.Context, _ *struct{}) (*empty, error) {
	if h.d.Updater == nil {
		return nil, unavailable("The updater")
	}
	h.updateMu.Lock()
	if h.updating != "" {
		h.updateMu.Unlock()
		return nil, huma.Error409Conflict("An update is being installed.")
	}
	h.updating = "applying"
	h.updateMu.Unlock()
	h.audit(ctx, "update.apply", "", nil)
	// The update goes on when the admin closes the page.
	err := h.d.Updater.Apply(context.WithoutCancel(ctx))
	next := "restarting"
	if err != nil {
		next = ""
	}
	h.updateMu.Lock()
	h.updating = next
	h.updateMu.Unlock()
	if err != nil {
		return nil, h.serviceErr(err)
	}
	return nil, nil
}

func (h *Handler) installScript(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
	if h.d.InstallScript == nil {
		return nil, unavailable("The install script")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		r, w := humago.Unwrap(hctx)
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(h.d.InstallScript.Script(r))
	}}, nil
}
