package image

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/update"
)

var runnerArches = []string{"x64", "arm64"}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type BundleConfig struct {
	Mode sandbox.Mode // in VM mode, each bundle also gets an ext4 drive
	Dir  string       // the runners folder
	// Token, if not nil, gives a GitHub token for the API calls.
	Token  func(context.Context) (string, error)
	Logger *slog.Logger
}

// Bundles downloads and patches the GitHub Actions runner.
type Bundles struct {
	cfg      BundleConfig
	log      *slog.Logger
	work     keyLock
	api, web string // GitHub base URLs; tests point them at a fake
}

func NewBundles(cfg BundleConfig) *Bundles {
	return &Bundles{
		cfg: cfg,
		log: cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler)),
		api: "https://api.github.com",
		web: "https://github.com",
	}
}

// Bundle is a runner that is ready on this server.
type Bundle struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"` // of the release archive
	// Patched reports if the runner keeps ACTIONS_RESULTS_URL from its .env
	// file, so that jobs use the runplane cache server.
	Patched  bool   `json:"patched"`
	Dir      string `json:"dir"`
	Ext4Path string `json:"ext4_path,omitempty"` // VM mode: the read-only drive
}

// Ensure makes the runner of version and arch ready in the runners folder. It
// downloads the release, checks its SHA-256, extracts it, and applies the
// runner patch (guest.Patch). If the patch does not apply, the bundle stays
// unpatched and jobs use the GitHub cache.
func (b *Bundles) Ensure(ctx context.Context, version, arch string) (Bundle, error) {
	if !versionRE.MatchString(version) || !slices.Contains(runnerArches, arch) {
		return Bundle{}, fmt.Errorf("ensure runner: bad version %q or arch %q", version, arch)
	}
	name := version + "-" + arch
	unlock, err := b.work.lock(ctx, name)
	if err != nil {
		return Bundle{}, err
	}
	defer unlock()
	bundle, err := b.ensure(ctx, version, arch)
	if err != nil {
		return Bundle{}, fmt.Errorf("ensure runner %s: %w", name, err)
	}
	return bundle, nil
}

// stamp is the file <name>.json next to a bundle folder. It exists only for
// a complete folder.
type stamp struct {
	SHA256  string `json:"sha256"`
	Patched bool   `json:"patched"`
}

func (b *Bundles) ensure(ctx context.Context, version, arch string) (Bundle, error) {
	name := version + "-" + arch
	bundle := Bundle{Version: version, Arch: arch, Dir: filepath.Join(b.cfg.Dir, name)}
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
		if st, err = b.fetch(ctx, version, arch, bundle.Dir); err != nil {
			return Bundle{}, err
		}
		if err := writeJSON(stampPath, st); err != nil {
			return Bundle{}, err
		}
	}
	bundle.SHA256, bundle.Patched = st.SHA256, st.Patched

	if b.cfg.Mode == sandbox.ModeVM {
		bundle.Ext4Path = bundle.Dir + ".ext4"
		if _, err := os.Stat(bundle.Ext4Path); err != nil {
			_, err = writeExt4(bundle.Ext4Path, func(w io.Writer) error { return tarDir(w, bundle.Dir) })
			if err != nil {
				return Bundle{}, err
			}
		}
	}
	return bundle, nil
}

// fetch downloads and checks the release archive, then extracts and patches
// it into dir.
func (b *Bundles) fetch(ctx context.Context, version, arch, dir string) (stamp, error) {
	_, sums, err := b.release(ctx, "/releases/tags/v"+version)
	if err != nil {
		return stamp{}, fmt.Errorf("read release: %w", err)
	}
	want := sums[arch]
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
	got, err := b.download(ctx, b.web+"/actions/runner/releases/download/v"+version+"/"+archiveName(version, arch), archive)
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

func archiveName(version, arch string) string {
	return "actions-runner-linux-" + arch + "-" + version + ".tar.gz"
}

// release reads a release of actions/runner and returns its version and the
// hex SHA-256 of the archive of each arch.
func (b *Bundles) release(ctx context.Context, path string) (string, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api+"/repos/actions/runner"+path, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if b.cfg.Token != nil {
		token, err := b.cfg.Token(ctx)
		if err != nil {
			return "", nil, fmt.Errorf("get GitHub token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	var body struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", nil, fmt.Errorf("GET %s: %w", path, err)
	}
	version := strings.TrimPrefix(body.TagName, "v")
	sums := map[string]string{}
	for _, arch := range runnerArches {
		sum := markedSHA(body.Body, "linux-"+arch)
		if sum == "" {
			// GitHub also gives the digest of each asset.
			for _, a := range body.Assets {
				if a.Name == archiveName(version, arch) {
					sum, _ = strings.CutPrefix(a.Digest, "sha256:")
				}
			}
		}
		if isHex(sum, 64) {
			sums[arch] = sum
		}
	}
	return version, sums, nil
}

// markedSHA reads the SHA-256 between the markers that the runner release
// notes have for each platform:
// <!-- BEGIN SHA linux-x64 -->hex<!-- END SHA linux-x64 -->
func markedSHA(body, platform string) string {
	_, rest, ok := strings.Cut(body, "<!-- BEGIN SHA "+platform+" -->")
	if !ok {
		return ""
	}
	sum, _, ok := strings.Cut(rest, "<!-- END SHA "+platform+" -->")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(sum))
}

// download writes the file at url to dst and returns its SHA-256 in hex.
func (b *Bundles) download(ctx context.Context, url, dst string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
	if !b.work.idle(func() { err = b.prune(keep) }) {
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
		if e.IsDir() && versionRE.MatchString(version) && slices.Contains(runnerArches, arch) {
			versions[arch] = append(versions[arch], version)
		}
	}
	for arch, list := range versions {
		slices.SortFunc(list, func(x, y string) int { return update.Compare(y, x) })
		for i, version := range list {
			if i < 2 || slices.Contains(keep, version) {
				continue
			}
			dir := filepath.Join(b.cfg.Dir, version+"-"+arch)
			// The stamp goes first, so that a partial removal is not a bundle.
			for _, p := range []string{dir + ".json", dir + ".ext4", dir} {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
			b.log.Info("removed old runner bundle", "version", version, "arch", arch)
		}
	}
	return nil
}

// writeJSON writes v to file through a temp file and a rename.
func writeJSON(file string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(file), "."+filepath.Base(file)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
