package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSetDefaultImage(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now()
	for _, img := range []Image{
		{ID: "i1", Name: "Ubuntu 24.04", Ref: "ghcr.io/runplane/ubuntu-24.04", IsDefault: true, Source: "builtin"},
		{ID: "i2", Name: "Ubuntu 26.04", Ref: "ghcr.io/runplane/ubuntu-26.04", Source: "builtin"},
	} {
		img.CreatedAt, img.UpdatedAt = now, now
		must(t, s.CreateImage(ctx, img))
	}
	defaultImage := func() string {
		t.Helper()
		list, err := s.ListImages(ctx)
		must(t, err)
		var ids []string
		for _, img := range list {
			if img.IsDefault {
				ids = append(ids, img.ID)
			}
		}
		return strings.Join(ids, ",")
	}
	must(t, s.SetDefaultImage(ctx, "i2", now))
	if id := defaultImage(); id != "i2" {
		t.Fatalf("default = %q", id)
	}
	if err := s.SetDefaultImage(ctx, "missing", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown image: err = %v", err)
	}
	if id := defaultImage(); id != "i2" {
		t.Fatalf("a failed change lost the default: %q", id)
	}

	// A pool with a deleted image changes to the default image.
	must(t, s.CreatePool(ctx, Pool{ID: "p", Name: "Default", Label: "runplane", Arch: "x64", VCPU: 2, MemoryMB: 4096,
		DiskGB: 64, ImageID: "i1", MaxRunners: 2, Enabled: true, CreatedAt: now, UpdatedAt: now}))
	must(t, s.DeleteImage(ctx, "i1"))
	if p, err := s.GetPool(ctx, "p"); err != nil || p.ImageID != "" {
		t.Fatalf("pool image = %q, %v; want empty", p.ImageID, err)
	}
}
