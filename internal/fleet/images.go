package fleet

import (
	"context"
	"errors"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

// ErrDefaultImage refuses the removal of the default image.
var ErrDefaultImage = errors.New("the default image cannot be removed; make another image the default first")

// CreateImage adds an image from a registry, and pulls it on each connected
// server. A refused value gives field.Problems, and a ref in use
// store.ErrConflict.
func (f *Fleet) CreateImage(ctx context.Context, by audit.Actor, imgName, ref string) (store.Image, error) {
	imgName, ref, err := checkImage(imgName, ref)
	if err != nil {
		return store.Image{}, err
	}
	now := f.now()
	img := store.Image{ID: ids.New(), Name: imgName, Ref: ref, Source: "registry", CreatedAt: now, UpdatedAt: now}
	if err := f.st.CreateImage(ctx, img); err != nil {
		return store.Image{}, err
	}
	f.audit.Record(ctx, by, audit.ImageCreate, img.ID, map[string]any{"name": img.Name, "ref": img.Ref})
	f.bus.Publish(events.ImageState(img))
	f.pullEverywhere(img)
	return img, nil
}

// ImageChange is a change of an image by an admin. A nil field keeps its
// value.
type ImageChange struct {
	Name *string
	Ref  *string // a new ref pulls the image again
	// Default true makes the image the default. To change the default, an
	// admin makes another image the default.
	Default *bool
}

func (f *Fleet) UpdateImage(ctx context.Context, by audit.Actor, imageID string, c ImageChange) (store.Image, error) {
	img, err := f.st.GetImage(ctx, imageID)
	if err != nil {
		return store.Image{}, err
	}
	var p field.Problems
	switch {
	case c.Default != nil && !*c.Default && img.IsDefault:
		p.Add("is_default", "Make another image the default instead.", false)
	case c.Ref != nil && *c.Ref != img.Ref && img.Source == images.Source:
		p.Add("ref", "The ref of a built-in image cannot change. Add a new image instead.", *c.Ref)
	}
	if err := p.Err(); err != nil {
		return store.Image{}, err
	}
	imgName, ref := img.Name, img.Ref
	if c.Name != nil {
		imgName = *c.Name
	}
	if c.Ref != nil {
		ref = *c.Ref
	}
	if imgName, ref, err = checkImage(imgName, ref); err != nil {
		return store.Image{}, err
	}
	refChanged := ref != img.Ref
	if imgName != img.Name || refChanged {
		img.Name, img.Ref, img.UpdatedAt = imgName, ref, f.now()
		if err := f.st.UpdateImage(ctx, img); err != nil {
			return store.Image{}, err
		}
	}
	if c.Default != nil && *c.Default && !img.IsDefault {
		if err := f.st.SetDefaultImage(ctx, img.ID, f.now()); err != nil {
			return store.Image{}, err
		}
		img.IsDefault = true
		// The pools without an image use the default image.
		f.bus.Publish(events.PoolsChanged{})
	}
	f.audit.Record(ctx, by, audit.ImageUpdate, img.ID, map[string]any{"name": img.Name, "ref": img.Ref, "is_default": c.Default})
	f.bus.Publish(events.ImageState(img))
	f.kick()
	if refChanged {
		f.pullEverywhere(img)
	}
	return img, nil
}

// RemoveImage stops the use of an image and deletes its row. The pools with
// the image change to the default image. The connected servers remove the
// files of the image in the background, because a server removes images
// only after its pulls end.
func (f *Fleet) RemoveImage(ctx context.Context, by audit.Actor, imageID string) error {
	img, err := f.st.GetImage(ctx, imageID)
	if err != nil {
		return err
	}
	if img.IsDefault {
		return ErrDefaultImage
	}
	f.mu.Lock()
	f.dropped[img.ID] = true
	delete(f.images, img.ID)
	var nodes []*node
	for _, n := range f.nodes {
		delete(n.images, img.ID)
		delete(n.failed, img.ID)
		nodes = append(nodes, n)
	}
	f.mu.Unlock()
	if err := f.st.DeleteImage(ctx, img.ID); err != nil {
		return err
	}
	for _, n := range nodes {
		f.goBg(func(ctx context.Context) { f.gcImages(ctx, n) })
	}
	f.bus.Publish(events.PoolsChanged{})
	f.bus.Publish(events.ImageRemoved{ID: img.ID})
	f.audit.Record(ctx, by, audit.ImageDelete, img.ID, map[string]any{"name": img.Name, "ref": img.Ref})
	return nil
}

// RefreshImage pulls the image again on each connected server.
func (f *Fleet) RefreshImage(ctx context.Context, by audit.Actor, imageID string) error {
	img, err := f.st.GetImage(ctx, imageID)
	if err != nil {
		return err
	}
	f.pullEverywhere(img)
	f.audit.Record(ctx, by, audit.ImageRefresh, img.ID, map[string]any{"ref": img.Ref})
	return nil
}

// After pullEverywhere, the servers keep the image that they have until the
// new pull is ready, unless its ref changed. The builds show a failure.
func (f *Fleet) pullEverywhere(img store.Image) {
	var list []*node
	f.mu.Lock()
	f.images[img.ID] = img
	if img.IsDefault {
		f.defaultID = img.ID
	}
	for _, n := range f.nodes {
		if n.pulls[img.ID] != img.Ref {
			n.pulls[img.ID] = img.Ref
			list = append(list, n)
		}
	}
	f.mu.Unlock()
	for _, n := range list {
		f.goBg(func(ctx context.Context) { f.pull(ctx, n, img) })
	}
}

func checkImage(imgName, ref string) (string, string, error) {
	imgName, ref = strings.TrimSpace(imgName), strings.TrimSpace(ref)
	var p field.Problems
	if imgName == "" {
		p.Add("name", "Enter a name.", imgName)
	}
	if _, err := name.ParseReference(ref); err != nil {
		p.Add("ref", "The ref is not a valid image reference, for example ghcr.io/owner/image:tag.", ref)
	}
	return imgName, ref, p.Err()
}
