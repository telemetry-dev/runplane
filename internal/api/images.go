package api

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/fleet"
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
				ServerID: b.ServerID, ServerName: r.servers[b.ServerID].Name, Arch: r.servers[b.ServerID].Arch,
				Digest: b.Digest, Status: b.Status, Error: b.Error, SizeBytes: b.SizeBytes, UpdatedAt: b.UpdatedAt,
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

func (h *Handler) listImages(ctx context.Context, _ *struct{}) (*body[[]Image], error) {
	list, err := h.d.Store.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	return reply(h.images(ctx, list))
}

func (h *Handler) imageOutput(ctx context.Context, id string) (*body[Image], error) {
	img, err := h.d.Store.GetImage(ctx, id)
	if err != nil {
		return nil, notFound(err, "No image has this ID.")
	}
	return reply(first(h.images(ctx, []store.Image{img})))
}

type CreateImageRequest struct {
	Name string `json:"name" minLength:"1" maxLength:"64"`
	Ref  string `json:"ref" minLength:"1" maxLength:"512" doc:"OCI reference. Build it FROM a runplane base image."`
}

func (h *Handler) createImage(ctx context.Context, in *body[CreateImageRequest]) (*body[Image], error) {
	if h.d.Images == nil {
		return nil, unavailable("Adding images")
	}
	img, err := h.d.Images.CreateImage(ctx, actor(ctx), in.Body.Name, in.Body.Ref)
	if err != nil {
		return nil, imageErr(err)
	}
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
}) (*body[Image], error) {
	if h.d.Images == nil {
		return nil, unavailable("Changing images")
	}
	b := in.Body
	img, err := h.d.Images.UpdateImage(ctx, actor(ctx), in.ID, fleet.ImageChange{Name: b.Name, Ref: b.Ref, Default: b.IsDefault})
	if err != nil {
		return nil, imageErr(err)
	}
	return h.imageOutput(ctx, img.ID)
}

func (h *Handler) deleteImage(ctx context.Context, in *PathID) (*empty, error) {
	if h.d.Images == nil {
		return nil, unavailable("Removing images")
	}
	err := h.d.Images.RemoveImage(ctx, actor(ctx), in.ID)
	if errors.Is(err, fleet.ErrDefaultImage) {
		return nil, huma.Error409Conflict("The default image cannot be removed. Make another image the default first.")
	}
	if err != nil {
		return nil, imageErr(err)
	}
	return nil, nil
}

func (h *Handler) refreshImage(ctx context.Context, in *PathID) (*empty, error) {
	if h.d.Images == nil {
		return nil, unavailable("Pulling images")
	}
	if err := h.d.Images.RefreshImage(ctx, actor(ctx), in.ID); err != nil {
		return nil, imageErr(err)
	}
	return nil, nil
}

func imageErr(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return conflict("body.ref", "An image with this ref exists.")
	}
	return notFound(changeErr(err), "No image has this ID.")
}
