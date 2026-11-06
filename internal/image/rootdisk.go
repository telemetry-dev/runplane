package image

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/ext4"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// ensureRootDisk pulls the image for this platform without Docker and makes
// its ext4 root disk. A root disk that is already there is used again.
func (m *Manager) ensureRootDisk(ctx context.Context, src source, r *reporter) (Image, error) {
	init, err := m.init()
	if err != nil {
		return Image{}, err
	}
	m.pruneOld.Do(func() { m.removeOldInitDisks(init.sum) })
	if err := os.MkdirAll(m.cfg.ImagesDir, 0o700); err != nil {
		return Image{}, err
	}
	r.status(StatusPulling)
	img, err := remoteImage(ctx, src.ref)
	if err != nil {
		if src.builtin == "" || m.cfg.Docker == nil {
			return Image{}, err
		}
		m.log.Warn("image pull failed, building the builtin image with Docker", "ref", src.ref, "err", err)
		return m.buildRootDisk(ctx, src, init.bin, r)
	}
	digest, err := img.Digest()
	if err != nil {
		return Image{}, fmt.Errorf("digest of %s: %w", src.ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Image{}, fmt.Errorf("config of %s: %w", src.ref, err)
	}
	out := Image{Ref: src.ref, Digest: digest.String()}
	if out.Path, err = m.rootDiskPath(out.Digest); err != nil {
		return Image{}, err
	}
	if st, err := os.Stat(out.Path); err == nil {
		out.Size = st.Size()
		return out, nil
	}

	tmp, err := os.MkdirTemp(m.cfg.ImagesDir, ".pull-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(tmp)
	layers, err := download(ctx, img, tmp, r)
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", src.ref, err)
	}

	r.status(StatusConverting)
	out.Size, err = ext4.Write(out.Path, func(w io.Writer) error {
		return rootfs(w, mutate.Extract(layersImage{img, layers}), init.bin, cfg.Config.Env)
	})
	if err != nil {
		return Image{}, err
	}
	return out, nil
}

// buildRootDisk builds a builtin image with Docker and makes the root disk
// from the files of a container of it.
func (m *Manager) buildRootDisk(ctx context.Context, src source, init []byte, r *reporter) (Image, error) {
	tag := src.localTag()
	r.status(StatusBuilding)
	if err := m.build(ctx, src.builtin, tag); err != nil {
		return Image{}, err
	}
	info, err := m.cfg.Docker.ImageInspect(ctx, tag)
	if err != nil {
		return Image{}, fmt.Errorf("inspect %s: %w", tag, err)
	}
	var env []string
	if info.Config != nil {
		env = info.Config.Env
	}
	out := Image{Ref: tag, Digest: info.ID}
	if out.Path, err = m.rootDiskPath(out.Digest); err != nil {
		return Image{}, err
	}
	if st, err := os.Stat(out.Path); err == nil {
		out.Size = st.Size()
		return out, nil
	}

	r.status(StatusConverting)
	c, err := m.cfg.Docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  tag,
			Cmd:    []string{guest.InitPath},
			Labels: map[string]string{guest.LabelManaged: "true"},
		},
	})
	if err != nil {
		return Image{}, fmt.Errorf("create container of %s: %w", tag, err)
	}
	defer m.cfg.Docker.ContainerRemove(context.WithoutCancel(ctx), c.ID, client.ContainerRemoveOptions{Force: true})
	files, err := m.cfg.Docker.ContainerExport(ctx, c.ID, client.ContainerExportOptions{})
	if err != nil {
		return Image{}, fmt.Errorf("export container of %s: %w", tag, err)
	}
	var read int64
	counted := &counter{ctx: ctx, r: files, add: func(n int64) {
		read += n
		r.bytes(StatusConverting, read, info.Size)
	}}
	out.Size, err = ext4.Write(out.Path, func(w io.Writer) error {
		return rootfs(w, readCloser{counted, files}, init, env)
	})
	if err != nil {
		return Image{}, err
	}
	return out, nil
}

func remoteImage(ctx context.Context, ref string) (v1.Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", ref, err)
	}
	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithUserAgent("runplane"),
	)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	if cfg.OS != "linux" || cfg.Architecture != runtime.GOARCH {
		return nil, fmt.Errorf("%s is for %s/%s, this server is linux/%s", ref, cfg.OS, cfg.Architecture, runtime.GOARCH)
	}
	return img, nil
}

// download copies the compressed layers of img to files in dir. The layers
// report the reads of the files as converting progress.
func download(ctx context.Context, img v1.Image, dir string, r *reporter) ([]fileLayer, error) {
	remoteLayers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	var total int64
	var read atomic.Int64
	layers := make([]fileLayer, len(remoteLayers))
	for i, l := range remoteLayers {
		size, err := l.Size()
		if err != nil {
			return nil, err
		}
		total += size
		layers[i] = fileLayer{Layer: l, path: filepath.Join(dir, strconv.Itoa(i)), read: func(n int64) {
			r.bytes(StatusConverting, read.Add(n), total)
		}}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var done atomic.Int64
	add := func(n int64) { r.bytes(StatusPulling, done.Add(n), total) }
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, l := range layers {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			if err := l.fetch(ctx, add); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return layers, nil
}

// fileLayer is a layer whose compressed bytes are in a local file. The
// remote layer gives the digest, the size, and the media type.
type fileLayer struct {
	v1.Layer
	path string
	read func(n int64)
}

// fetch needs no digest check: the remote reader returns an error at the end
// if the digest is not correct.
func (l fileLayer) fetch(ctx context.Context, add func(int64)) error {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, &counter{ctx: ctx, r: rc, add: add}); err != nil {
		return err
	}
	return f.Close()
}

func (l fileLayer) Compressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	return readCloser{&counter{ctx: context.Background(), r: f, add: l.read}, f}, nil
}

// layersImage is img with the layers from local files.
type layersImage struct {
	v1.Image
	layers []fileLayer
}

func (i layersImage) Layers() ([]v1.Layer, error) {
	out := make([]v1.Layer, len(i.layers))
	for n, l := range i.layers {
		// CompressedToLayer decompresses Compressed, so the
		// remote Uncompressed does not download the layer again.
		layer, err := partial.CompressedToLayer(l)
		if err != nil {
			return nil, err
		}
		out[n] = layer
	}
	return out, nil
}

// rootfs copies the tar of a flat file system from src to w, and adds the
// mount points, runplane-init, and the image environment env. The ext4
// writer cannot write through a symlink, so rootfs follows symlinks such as
// /sbin -> usr/sbin.
func rootfs(w io.Writer, src io.ReadCloser, init []byte, env []string) error {
	defer src.Close()
	tr := tar.NewReader(src)
	tw := tar.NewWriter(w)
	links := map[string]string{}
	have := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read image files: %w", err)
		}
		name := clean(hdr.Name)
		if name == ".dockerenv" {
			continue
		}
		have[name] = true
		if hdr.Typeflag == tar.TypeSymlink {
			links[name] = hdr.Linkname
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("read image files: %w", err)
		}
	}
	for _, mp := range guest.MountPoints {
		p := resolve(strings.TrimPrefix(mp.Path, "/"), links)
		if p == "" || have[p] {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: mp.Mode}); err != nil {
			return err
		}
	}
	files := []struct {
		path string
		mode int64
		data []byte
	}{
		{guest.InitPath, 0o755, init},
		{guest.ImageEnvFile, 0o644, guest.EncodeImageEnv(env)},
	}
	for _, f := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: resolve(strings.TrimPrefix(f.path, "/"), links), Mode: f.mode, Size: int64(len(f.data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// resolve follows the symlinks in links through each part of p, as the
// guest kernel does. Paths are relative to the root and cannot leave it.
func resolve(p string, links map[string]string) string {
	todo := strings.Split(p, "/")
	dir := ""
	for hops := 0; len(todo) > 0; {
		next := clean(path.Join(dir, todo[0]))
		todo = todo[1:]
		target, ok := links[next]
		if !ok || hops == 40 {
			dir = next
			continue
		}
		hops++
		if path.IsAbs(target) {
			dir = ""
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	return dir
}

// clean makes p relative to the root, without "..", ".", or a leading "/".
func clean(p string) string {
	return path.Clean("/" + p)[1:]
}

// removeOldInitDisks removes the root disks of an earlier runplane-init. No
// sandbox starts from them again, and a running VM has a hard link of its
// root disk. The fleet runs diskGC only when a digest changes, so without
// this a runplane update with a new init left a copy of each image.
func (m *Manager) removeOldInitDisks(sum string) {
	entries, err := os.ReadDir(m.cfg.ImagesDir)
	if err != nil {
		m.log.Warn("list root disks", "err", err)
		return
	}
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".ext4")
		digest, old, found := strings.Cut(stem, "-")
		if !ok || !found || !isHex(digest, 64) || !isHex(old, 8) || old == sum {
			continue
		}
		p := filepath.Join(m.cfg.ImagesDir, e.Name())
		if err := os.Remove(p); err != nil {
			m.log.Warn("remove root disk of an earlier runplane-init", "path", p, "err", err)
			continue
		}
		m.log.Info("removed root disk of an earlier runplane-init", "path", p)
	}
}

// diskGC removes the root disks that keep does not hold, the root disks of
// an earlier runplane-init, and the files of stopped conversions.
func (m *Manager) diskGC(keep map[string]bool) error {
	init, err := m.init()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(m.cfg.ImagesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(m.cfg.ImagesDir, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			continue
		}
		stem, ok := strings.CutSuffix(e.Name(), ".ext4")
		if !ok {
			continue
		}
		digest, sum, _ := strings.Cut(stem, "-")
		if sum == init.sum && keep["sha256:"+digest] {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		m.log.Info("removed unused root disk", "path", p)
	}
	return nil
}
