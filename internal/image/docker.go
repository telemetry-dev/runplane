package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

var platform = ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}

// ensureDocker pulls the image with Docker. A builtin image that Docker
// cannot pull is built from its embedded Dockerfile.
func (m *Manager) ensureDocker(ctx context.Context, src source, r *reporter) (Image, error) {
	ref := src.ref
	r.status(StatusPulling)
	if err := m.pull(ctx, ref, r); err != nil {
		if src.builtin == "" {
			// An image that the user built on this server has no registry.
			img, ierr := m.inspect(ctx, ref)
			if ierr != nil {
				return Image{}, err
			}
			m.log.Warn("image pull failed, using the local image", "ref", ref, "err", err)
			return img, nil
		}
		m.log.Warn("image pull failed, building the builtin image", "ref", ref, "err", err)
		ref = src.localTag()
		r.status(StatusBuilding)
		if err := m.build(ctx, src.builtin, ref); err != nil {
			return Image{}, err
		}
	}
	return m.inspect(ctx, ref)
}

func (m *Manager) inspect(ctx context.Context, ref string) (Image, error) {
	info, err := m.cfg.Docker.ImageInspect(ctx, ref)
	if err != nil {
		return Image{}, fmt.Errorf("inspect %s: %w", ref, err)
	}
	return Image{Ref: ref, Digest: info.ID, Size: info.Size}, nil
}

func (m *Manager) pull(ctx context.Context, ref string, r *reporter) error {
	resp, err := m.cfg.Docker.ImagePull(ctx, ref, client.ImagePullOptions{
		Platforms: []ocispec.Platform{platform},
	})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	layers := map[string]jsonstream.Progress{}
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if msg.Error != nil {
			return fmt.Errorf("pull %s: %s", ref, msg.Error.Message)
		}
		layer, seen := layers[msg.ID]
		switch {
		case msg.Status == "Downloading" && msg.Progress != nil:
			layer = *msg.Progress
		case msg.Status == "Download complete" && seen:
			layer.Current = layer.Total
		default:
			continue
		}
		layers[msg.ID] = layer
		var done, total int64
		for _, l := range layers {
			done += l.Current
			total += l.Total
		}
		r.bytes(StatusPulling, done, total)
	}
	return nil
}

func (m *Manager) build(ctx context.Context, name, tag string) error {
	args := map[string]*string{}
	for k, v := range images.BuildArgs(name) {
		args[k] = &v
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() { pw.CloseWithError(writeContext(pw, images.FS)) }()
	res, err := m.cfg.Docker.ImageBuild(ctx, pr, client.ImageBuildOptions{
		Tags:      []string{tag},
		BuildArgs: args,
		Version:   build.BuilderBuildKit,
		Remove:    true,
		Platforms: []ocispec.Platform{platform},
	})
	if err != nil {
		return fmt.Errorf("build %s: %w", name, err)
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var msg jsonstream.Message
		err := dec.Decode(&msg)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
		if msg.Error != nil {
			return fmt.Errorf("build %s: %s", name, msg.Error.Message)
		}
	}
}

// writeContext writes dir as a tar build context. embed.FS keeps no file
// modes, so a file that starts with "#!" becomes executable.
func writeContext(w io.Writer, dir fs.FS) error {
	tw := tar.NewWriter(w)
	err := fs.WalkDir(dir, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o755})
		}
		b, err := fs.ReadFile(dir, p)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if bytes.HasPrefix(b, []byte("#!")) {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: mode, Size: int64(len(b))}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// dockerGC removes the images with the runplane label that keep does not
// hold. Docker refuses to remove an image that a container uses; GC leaves it.
func (m *Manager) dockerGC(ctx context.Context, keep map[string]bool) error {
	list, err := m.cfg.Docker.ImageList(ctx, client.ImageListOptions{
		Filters: client.Filters{}.Add("label", guest.LabelManaged+"=true"),
	})
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	for _, img := range list.Items {
		if keep[img.ID] {
			continue
		}
		if _, err := m.cfg.Docker.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{PruneChildren: true}); err != nil {
			m.log.Debug("keep image", "id", img.ID, "err", err)
			continue
		}
		m.log.Info("removed unused image", "id", img.ID, "tags", img.RepoTags)
	}
	return nil
}
