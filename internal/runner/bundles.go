// Package runner makes the GitHub Actions runner ready on a server. It reads
// the releases of actions/runner, downloads and examines the archive,
// applies the runner patch (guest.Patch), and makes the ext4 drive of VM
// mode. The subpackage versions keeps the runner versions of all servers.
package runner

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/ext4"
	"github.com/runplane/runplane/internal/keylock"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/version"
)

type BundleConfig struct {
	Mode     sandbox.Mode // in VM mode, each bundle also gets an ext4 drive
	Dir      string       // the runners folder
	Releases Releases
	Logger   *slog.Logger
}

// Bundles downloads and patches the GitHub Actions runner.
type Bundles struct {
	cfg  BundleConfig
	log  *slog.Logger
	work keylock.Map
}

func NewBundles(cfg BundleConfig) *Bundles {
	return &Bundles{cfg: cfg, log: cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler))}
}

// Bundle is a runner that is ready on this server.
type Bundle struct {
	Version string
	Arch    string
	SHA256  string // of the release archive
	// Patched reports if the runner keeps ACTIONS_RESULTS_URL from its .env
	// file, so that jobs use the runplane cache server.
	Patched  bool
	Dir      string
	Ext4Path string // VM mode: the read-only drive
}

// Ensure makes the runner of version and arch ready in the runners folder. It
// downloads the release, examines its SHA-256, extracts it, and applies the
// runner patch (guest.Patch). If the patch does not apply, the bundle stays
// unpatched and jobs use the GitHub cache.
func (b *Bundles) Ensure(ctx context.Context, version, arch string) (Bundle, error) {
	if !versionRE.MatchString(version) || !slices.Contains(Arches, arch) {
		return Bundle{}, fmt.Errorf("ensure runner: bad version %q or arch %q", version, arch)
	}
	bundle := Bundle{Version: version, Arch: arch, Dir: b.dir(version, arch)}
	unlock, err := b.work.Lock(ctx, bundle.Dir)
	if err != nil {
		return Bundle{}, err
	}
	defer unlock()
	if err := b.ensure(ctx, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("ensure runner %s-%s: %w", version, arch, err)
	}
	return bundle, nil
}

// dir is the folder of a bundle. Next to it are the stamp <dir>.json, which
// exists only for a complete folder, and in VM mode the drive <dir>.ext4.
func (b *Bundles) dir(version, arch string) string {
	return filepath.Join(b.cfg.Dir, version+"-"+arch)
}

type stamp struct {
	SHA256  string `json:"sha256"`
	Patched bool   `json:"patched"`
}

func (b *Bundles) ensure(ctx context.Context, bundle *Bundle) error {
	stampPath := bundle.Dir + ".json"
	var st stamp
	data, err := os.ReadFile(stampPath)
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err == nil {
		_, err = os.Stat(bundle.Dir)
	}
	if err != nil {
		if st, err = b.fetch(ctx, bundle.Version, bundle.Arch, bundle.Dir); err != nil {
			return err
		}
		data, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if err := atomicfile.Write(stampPath, bytes.NewReader(data), 0o644); err != nil {
			return err
		}
	}
	bundle.SHA256, bundle.Patched = st.SHA256, st.Patched

	if b.cfg.Mode == sandbox.ModeVM {
		bundle.Ext4Path = bundle.Dir + ".ext4"
		if _, err := os.Stat(bundle.Ext4Path); err != nil {
			_, err = ext4.Write(bundle.Ext4Path, func(w io.Writer) error { return ext4.TarDir(w, bundle.Dir) })
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// fetch replaces dir with the extracted and patched release archive.
func (b *Bundles) fetch(ctx context.Context, version, arch, dir string) (stamp, error) {
	rel, err := b.cfg.Releases.Get(ctx, version)
	if err != nil {
		return stamp{}, fmt.Errorf("read release: %w", err)
	}
	want := rel.SHA256[arch]
	if want == "" {
		return stamp{}, fmt.Errorf("release v%s has no SHA-256 for linux-%s", version, arch)
	}
	if err := os.MkdirAll(b.cfg.Dir, 0o755); err != nil {
		return stamp{}, err
	}
	tmp, err := os.MkdirTemp(b.cfg.Dir, "."+filepath.Base(dir)+".tmp-")
	if err != nil {
		return stamp{}, err
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "runner.tar.gz")
	got, err := b.cfg.Releases.download(ctx, version, arch, archive)
	if err != nil {
		return stamp{}, err
	}
	if got != want {
		return stamp{}, fmt.Errorf("SHA-256 of %s is %s, the release says %s", archiveName(version, arch), got, want)
	}
	root := filepath.Join(tmp, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		return stamp{}, err
	}
	if err := extract(archive, root); err != nil {
		return stamp{}, err
	}
	n, err := guest.Patch(root)
	if err != nil {
		return stamp{}, err
	}
	if n == 0 {
		b.log.Warn("runner patch does not apply, jobs use the GitHub cache", "version", version, "arch", arch)
	}
	if err := openDirs(root); err != nil {
		return stamp{}, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return stamp{}, err
	}
	if err := os.Rename(root, dir); err != nil {
		return stamp{}, err
	}
	b.log.Info("runner bundle ready", "version", version, "arch", arch, "patches", n)
	return stamp{SHA256: want, Patched: n > 0}, nil
}

// extract unpacks the runner archive into dir. os.Root keeps each path of
// the archive inside dir. Files get their mode from the archive, whatever
// the umask is.
func extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read runner archive: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read runner archive: %w", err)
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = extractFile(root, name, hdr.FileInfo().Mode().Perm(), tr)
		case tar.TypeSymlink:
			err = root.Symlink(hdr.Linkname, name)
		case tar.TypeLink:
			err = root.Link(path.Clean(hdr.Linkname), name)
		}
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}
}

func extractFile(root *os.Root, name string, mode os.FileMode, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	return f.Close()
}

// openDirs gives each folder in dir mode 0755, whatever the umask is.
// Sandboxes read the bundle as other users.
func openDirs(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o755)
	})
}

// Prune removes the bundles of each arch except the two newest and the
// versions in keep. It does nothing while Ensure operates.
func (b *Bundles) Prune(keep []string) error {
	var err error
	if !b.work.Idle(func() { err = b.prune(keep) }) {
		b.log.Debug("runner prune skipped, a bundle is in progress")
	}
	return err
}

func (b *Bundles) prune(keep []string) error {
	entries, err := os.ReadDir(b.cfg.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	versions := map[string][]string{} // by arch
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			// The temp folder of an Ensure call that stopped.
			if err := os.RemoveAll(filepath.Join(b.cfg.Dir, e.Name())); err != nil {
				return err
			}
			continue
		}
		version, arch, _ := strings.Cut(e.Name(), "-")
		if e.IsDir() && versionRE.MatchString(version) && slices.Contains(Arches, arch) {
			versions[arch] = append(versions[arch], version)
		}
	}
	for arch, list := range versions {
		slices.SortFunc(list, func(x, y string) int { return version.Compare(y, x) })
		for i, v := range list {
			if i < 2 || slices.Contains(keep, v) {
				continue
			}
			dir := b.dir(v, arch)
			// The stamp goes first, so that a partial removal is not a bundle.
			for _, p := range []string{dir + ".json", dir + ".ext4", dir} {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
			b.log.Info("removed old runner bundle", "version", v, "arch", arch)
		}
	}
	return nil
}
