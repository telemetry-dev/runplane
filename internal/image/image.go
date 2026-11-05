// Package image makes sandbox images and runner bundles ready on a server.
//
// In container and dev mode, Docker pulls or builds each image. In VM mode,
// each image becomes a read-only ext4 root disk that holds runplane-init, so
// the server does not need Docker.
package image

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/blob"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/version"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusPulling    Status = "pulling"
	StatusBuilding   Status = "building"
	StatusConverting Status = "converting"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
)

// Progress is one step of Manager.Ensure.
type Progress struct {
	Status Status `json:"status"`
	Done   int64  `json:"done"`  // bytes, 0 when unknown
	Total  int64  `json:"total"` // bytes, 0 when unknown
	Error  string `json:"error,omitempty"`
}

// Image is an image that is ready on this server.
type Image struct {
	Ref string `json:"ref"` // the reference that runplane pulled or built
	// Digest identifies the image on this server: the Docker image ID in
	// container and dev mode and after a local build, else the manifest
	// digest for this platform.
	Digest string `json:"digest"`
	Path   string `json:"path,omitempty"` // VM mode: the ext4 root disk
	Size   int64  `json:"size"`
}

type Config struct {
	Mode      sandbox.Mode
	ImagesDir string // VM mode: the root disks
	// Docker is necessary in container and dev mode. In VM mode it is
	// optional: with it, runplane can build a builtin image that it cannot pull.
	Docker *client.Client
	Init   func() ([]byte, error) // VM mode: the runplane-init binary
	Logger *slog.Logger
}

// Manager makes images ready on this server.
type Manager struct {
	cfg  Config
	log  *slog.Logger
	init func() (initFile, error)
	work keyLock

	freshMu sync.Mutex
	fresh   map[string]bool // digests that Ensure returned since the last GC

	pruneOld sync.Once // removes the root disks of an earlier runplane-init
}

type initFile struct {
	bin []byte
	sum string // the first 8 hex digits of the SHA-256
}

func New(cfg Config) (*Manager, error) {
	switch cfg.Mode {
	case sandbox.ModeContainer, sandbox.ModeDev:
		if cfg.Docker == nil {
			return nil, fmt.Errorf("new image manager: %s mode needs a Docker client", cfg.Mode)
		}
	case sandbox.ModeVM:
		if cfg.Init == nil || cfg.ImagesDir == "" {
			return nil, errors.New("new image manager: VM mode needs Init and ImagesDir")
		}
	default:
		return nil, fmt.Errorf("new image manager: unknown mode %q", cfg.Mode)
	}
	m := &Manager{cfg: cfg, log: cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler)), fresh: map[string]bool{}}
	// Only VM mode reads runplane-init.
	m.init = sync.OnceValues(func() (initFile, error) {
		bin, err := cfg.Init()
		if err != nil {
			return initFile{}, fmt.Errorf("read runplane-init: %w", err)
		}
		sum := sha256.Sum256(bin)
		return initFile{bin: bin, sum: hex.EncodeToString(sum[:4])}, nil
	})
	return m, nil
}

// Ensure makes the image ready on this server and returns it. ref is an OCI
// reference, or "builtin:<name>" for a folder of the images package, for
// example "builtin:ubuntu-24.04". A builtin image comes from
// ghcr.io/<version.Repo>/runner-<name>:latest, or from a local build when
// that pull fails. progress, if not nil, gets each step. Calls for the same
// ref wait for each other.
func (m *Manager) Ensure(ctx context.Context, ref string, progress func(Progress)) (Image, error) {
	r := &reporter{fn: progress}
	r.status(StatusPending)
	start := time.Now()
	img, err := m.ensure(ctx, ref, r)
	if err != nil {
		err = fmt.Errorf("ensure image %s: %w", ref, err)
		r.send(Progress{Status: StatusFailed, Error: err.Error()})
		return Image{}, err
	}
	m.freshMu.Lock()
	m.fresh[img.Digest] = true
	m.freshMu.Unlock()
	m.log.Info("image ready", "ref", ref, "from", img.Ref, "digest", img.Digest, "took", time.Since(start).Round(time.Millisecond))
	r.status(StatusReady)
	return img, nil
}

func (m *Manager) ensure(ctx context.Context, ref string, r *reporter) (Image, error) {
	src, err := m.source(ref)
	if err != nil {
		return Image{}, err
	}
	unlock, err := m.work.lock(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	defer unlock()
	if m.cfg.Mode == sandbox.ModeVM {
		return m.ensureRootDisk(ctx, src, r)
	}
	return m.ensureDocker(ctx, src, r)
}

// RootDisk returns the ext4 root disk of an image that is ready (VM mode).
func (m *Manager) RootDisk(digest string) (string, error) {
	path, err := m.rootDiskPath(digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("root disk of %s: %w", digest, err)
	}
	return path, nil
}

// GC removes the root disks (VM mode) and the runplane Docker images whose
// digest is not in keep. keep must hold the digests of all images and of all
// sandboxes that still operate. GC also keeps the images that Ensure returned
// since the last GC, because the caller can still be about to store them. GC
// does nothing while Ensure operates.
func (m *Manager) GC(ctx context.Context, keep []string) error {
	var err error
	ran := m.work.idle(func() {
		m.freshMu.Lock()
		set := m.fresh
		m.fresh = map[string]bool{}
		m.freshMu.Unlock()
		for _, d := range keep {
			set[d] = true
		}
		if m.cfg.Mode == sandbox.ModeVM {
			err = m.diskGC(set)
		}
		if err == nil && m.cfg.Docker != nil {
			err = m.dockerGC(ctx, set)
		}
	})
	if !ran {
		m.log.Debug("image GC skipped, an image is in progress")
	}
	return err
}

// source is where an image comes from.
type source struct {
	ref     string // the registry reference to pull
	builtin string // the builtin image name, empty for other images
}

func (m *Manager) source(ref string) (source, error) {
	name, ok := strings.CutPrefix(ref, "builtin:")
	if !ok {
		return source{ref: ref}, nil
	}
	if !fs.ValidPath(name) || strings.Contains(name, "/") {
		return source{}, fmt.Errorf("unknown builtin image %q", name)
	}
	if _, err := fs.Stat(images.FS, name+"/Dockerfile"); err != nil {
		return source{}, fmt.Errorf("unknown builtin image %q", name)
	}
	return source{ref: BuiltinRef(name), builtin: name}, nil
}

// BuiltinRef is the registry reference of the builtin image name, which the
// images workflow of the release repository publishes.
func BuiltinRef(name string) string {
	return "ghcr.io/" + strings.ToLower(version.Repo) + "/runner-" + name + ":latest"
}

// CanPull reads the manifest and the config of ref for this platform, as
// a pull in VM mode does first. It returns nil if the pull can start.
func CanPull(ctx context.Context, ref string) error {
	_, err := remoteImage(ctx, ref)
	var te *transport.Error
	if errors.As(err, &te) && len(te.Errors) > 0 {
		return fmt.Errorf("the registry answered %d %s: %s", te.StatusCode, te.Errors[0].Code, te.Errors[0].Message)
	}
	return err
}

// localTag is the tag of a builtin image that runplane built on this server.
func (s source) localTag() string {
	return "runplane/runner-" + s.builtin + ":local"
}

func (m *Manager) rootDiskPath(digest string) (string, error) {
	if !blob.ValidDigest(digest) {
		return "", fmt.Errorf("bad image digest %q", digest)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	init, err := m.init()
	if err != nil {
		return "", err
	}
	return filepath.Join(m.cfg.ImagesDir, hexDigest+"-"+init.sum+".ext4"), nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
