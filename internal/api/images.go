package api

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

type Image struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Ref       string       `json:"ref" doc:"OCI reference, for example \"ghcr.io/runplane/ubuntu-24.04:latest\"."`
	IsDefault bool         `json:"is_default" doc:"Pools without an image use the default image."`
	Source    string       `json:"source" enum:"registry,builtin"`
	PoolCount int          `json:"pool_count" doc:"Pools that use the image. For the default image, also the pools without an image."`
	Builds    []ImageBuild `json:"builds" doc:"The state of the image on each server."`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type ImageBuild struct {
	ServerID   string    `json:"server_id"`
	ServerName string    `json:"server_name"`
	Arch       string    `json:"arch" doc:"Arch of the server: x64 or arm64. Empty until the server reports its facts."`
	Digest     string    `json:"digest"`
	Status     string    `json:"status" enum:"pending,pulling,building,converting,ready,failed"`
	Error      string    `json:"error"`
	SizeBytes  int64     `json:"size_bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (h *Handler) images(ctx context.Context, list []store.Image) ([]Image, error) {
	builds, err := h.d.Store.ListImageBuilds(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Image, len(list))
	byID := map[string]*Image{}
	for i, img := range list {
		out[i] = Image{
			ID: img.ID, Name: img.Name, Ref: img.Ref, IsDefault: img.IsDefault, Source: img.Source,
			Builds: []ImageBuild{}, CreatedAt: img.CreatedAt, UpdatedAt: img.UpdatedAt,
		}
		byID[img.ID] = &out[i]
	}
	for _, b := range builds {
		if img := byID[b.ImageID]; img != nil {
			img.Builds = append(img.Builds, ImageBuild{
				ServerID: b.ServerID, ServerName: r.serverName(b.ServerID), Arch: r.servers[b.ServerID].Arch,
				Digest: b.Digest, Status: b.Status,
				Error: b.Error, SizeBytes: b.SizeBytes, UpdatedAt: b.UpdatedAt,
			})
		}
	}
	for _, p := range r.pools {
		for i := range out {
			if p.ImageID == out[i].ID || (p.ImageID == "" && out[i].IsDefault) {
				out[i].PoolCount++
			}
		}
	}
	return out, nil
}

func (h *Handler) listImages(ctx context.Context, _ *struct{}) (*struct{ Body []Image }, error) {
	list, err := h.d.Store.ListImages(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	out, err := h.images(ctx, list)
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body []Image }{out}, nil
}

func (h *Handler) imageOutput(ctx context.Context, id string) (*struct{ Body Image }, error) {
	img, err := h.d.Store.GetImage(ctx, id)
	if err != nil {
		return nil, h.storeErr(err, "No image has this ID.")
	}
	out, err := h.images(ctx, []store.Image{img})
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body Image }{out[0]}, nil
}

type CreateImageRequest struct {
	Name string `json:"name" minLength:"1" maxLength:"64"`
	Ref  string `json:"ref" minLength:"1" maxLength:"512" doc:"OCI reference. Build it FROM a runplane base image."`
}

func (h *Handler) createImage(ctx context.Context, in *struct{ Body CreateImageRequest }) (*struct{ Body Image }, error) {
	imgName, ref, err := checkImage(in.Body.Name, in.Body.Ref)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	img := store.Image{ID: ids.New(), Name: imgName, Ref: ref, Source: "registry", CreatedAt: now, UpdatedAt: now}
	if err := h.d.Store.CreateImage(ctx, img); err != nil {
		return nil, h.imageErr(err)
	}
	h.audit(ctx, "image.create", img.ID, map[string]any{"name": img.Name, "ref": img.Ref})
	h.d.Bus.Publish(events.Event{Type: events.ImageState, Data: img})
	h.refresh(ctx, img.ID)
	return h.imageOutput(ctx, img.ID)
}

type UpdateImageRequest struct {
	Name      *string `json:"name,omitempty" minLength:"1" maxLength:"64"`
	Ref       *string `json:"ref,omitempty" minLength:"1" maxLength:"512" doc:"A new ref pulls the image again."`
	IsDefault *bool   `json:"is_default,omitempty" doc:"true makes this image the default. To change the default, make another image the default."`
}

func (h *Handler) updateImage(ctx context.Context, in *struct {
	PathID
	Body UpdateImageRequest
}) (*struct{ Body Image }, error) {
	img, err := h.d.Store.GetImage(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No image has this ID.")
	}
	b := in.Body
	if b.IsDefault != nil && !*b.IsDefault && img.IsDefault {
		return nil, invalid("body.is_default", "Make another image the default instead.", false)
	}
	if b.Ref != nil && *b.Ref != img.Ref && img.Source == "builtin" {
		return nil, invalid("body.ref", "The ref of a built-in image cannot change. Add a new image instead.", *b.Ref)
	}
	newName, ref, err := checkImage(cmp.Or(ptrValue(b.Name), img.Name), cmp.Or(ptrValue(b.Ref), img.Ref))
	if err != nil {
		return nil, err
	}
	refChanged := ref != img.Ref
	if newName != img.Name || refChanged {
		img.Name, img.Ref = newName, ref
		if err := h.d.Store.UpdateImage(ctx, img); err != nil {
			return nil, h.imageErr(err)
		}
	}
	if b.IsDefault != nil && *b.IsDefault && !img.IsDefault {
		if err := h.d.Store.SetDefaultImage(ctx, img.ID); err != nil {
			return nil, h.imageErr(err)
		}
		h.poolsChanged(ctx)
	}
	h.audit(ctx, "image.update", img.ID, map[string]any{"name": img.Name, "ref": img.Ref, "is_default": b.IsDefault})
	h.d.Bus.Publish(events.Event{Type: events.ImageState, Data: img})
	if refChanged {
		h.refresh(ctx, img.ID)
	}
	return h.imageOutput(ctx, img.ID)
}

func ptrValue[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func (h *Handler) deleteImage(ctx context.Context, in *PathID) (*empty, error) {
	img, err := h.d.Store.GetImage(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No image has this ID.")
	}
	if img.IsDefault {
		return nil, huma.Error409Conflict("The default image cannot be removed. Make another image the default first.")
	}
	if h.d.Images == nil {
		return nil, unavailable("Removing images")
	}
	if err := h.d.Images.Remove(ctx, img.ID); err != nil {
		return nil, h.serviceErr(err)
	}
	// Pools with this image change to the default image.
	if err := h.d.Store.DeleteImage(ctx, img.ID); err != nil {
		return nil, h.storeErr(err, "No image has this ID.")
	}
	h.poolsChanged(ctx)
	h.audit(ctx, "image.delete", img.ID, map[string]any{"name": img.Name, "ref": img.Ref})
	return nil, nil
}

func (h *Handler) refreshImage(ctx context.Context, in *PathID) (*empty, error) {
	if h.d.Images == nil {
		return nil, unavailable("Pulling images")
	}
	img, err := h.d.Store.GetImage(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No image has this ID.")
	}
	if err := h.d.Images.Refresh(ctx, img.ID); err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "image.refresh", img.ID, map[string]any{"ref": img.Ref})
	return nil, nil
}

// refresh starts the pulls of a new or changed image. The builds show a
// failure, so a failure here does not fail the request.
func (h *Handler) refresh(ctx context.Context, id string) {
	if h.d.Images == nil {
		return
	}
	if err := h.d.Images.Refresh(context.WithoutCancel(ctx), id); err != nil {
		h.d.Log.Warn("pull image", "image", id, "err", err)
	}
}

// checkImage trims the name and the ref and checks the ref syntax.
func checkImage(imgName, ref string) (string, string, error) {
	imgName, ref = strings.TrimSpace(imgName), strings.TrimSpace(ref)
	var errs problems
	if imgName == "" {
		errs.add("body.name", "Enter a name.", imgName)
	}
	if _, err := name.ParseReference(ref); err != nil {
		errs.add("body.ref", "The ref is not a valid image reference, for example ghcr.io/owner/image:tag.", ref)
	}
	return imgName, ref, errs.err()
}

func (h *Handler) imageErr(err error) error {
	if errors.Is(err, store.ErrConflict) {
		msg := "An image with this ref exists."
		return huma.Error409Conflict(msg, &huma.ErrorDetail{Location: "body.ref", Message: msg})
	}
	return h.storeErr(err, "No image has this ID.")
}
