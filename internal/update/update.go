// Package update replaces the runplane binary with a signed GitHub release.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/version"
)

// PublicKey is the base64 of the 32-byte ed25519 public key that signs the
// SHA256SUMS file of each release. The release build sets it with
// -ldflags "-X github.com/runplane/runplane/internal/update.PublicKey=...".
// To get the value from an OpenSSL key:
//
//	openssl pkey -in release.pem -pubout -outform DER | tail -c 32 | base64
var PublicKey = ""

// Size limits for the downloads.
const (
	maxMetaSize = 1 << 20   // release JSON, SHA256SUMS, and its signature
	maxFileSize = 512 << 20 // an archive or a guest kernel
)

// ErrNoRelease means that the release repository has no published release.
var ErrNoRelease = errors.New("no published release")

// Updater checks for and applies releases.
type Updater struct {
	Repo        string // GitHub repository, "owner/name"
	Current     string // version of the running binary
	PublicKey   string // base64 ed25519 key; see the PublicKey variable
	APIURL      string // https://api.github.com if empty
	DownloadURL string // https://github.com if empty
	Exe         string // binary to replace; the running executable if empty
	// Kernel is the guest kernel of VM mode. If the file exists, Apply also
	// replaces it with vmlinux-<arch> of the release.
	Kernel string
}

// New returns an Updater for this build. kernel is the path of the guest
// kernel of VM mode.
func New(kernel string) *Updater {
	return &Updater{Repo: version.Repo, Current: version.Version, PublicKey: PublicKey, Kernel: kernel}
}

// Status is the result of Check.
type Status struct {
	Current     string
	Latest      string // tag of the latest release, for example "v0.5.0"
	Available   bool   // Latest is newer than Current
	URL         string // release page
	Notes       string // release notes (Markdown)
	PublishedAt time.Time
}

// Check reads the latest release. Only a release build can have an update
// available.
func (u *Updater) Check(ctx context.Context) (Status, error) {
	var rel struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		Body        string    `json:"body"`
		PublishedAt time.Time `json:"published_at"`
	}
	url := cmp.Or(u.APIURL, "https://api.github.com") + "/repos/" + u.Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Status{}, fmt.Errorf("check update: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "runplane/"+u.Current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("check update: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Status{}, fmt.Errorf("check update: %s has %w", u.Repo, ErrNoRelease)
	case resp.StatusCode != http.StatusOK:
		return Status{}, fmt.Errorf("check update: GitHub answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaSize)).Decode(&rel); err != nil {
		return Status{}, fmt.Errorf("check update: decode release: %w", err)
	}
	return Status{
		Current:     u.Current,
		Latest:      rel.TagName,
		Available:   version.IsRelease(u.Current) && version.Compare(rel.TagName, u.Current) > 0,
		URL:         rel.HTMLURL,
		Notes:       rel.Body,
		PublishedAt: rel.PublishedAt,
	}, nil
}

// Apply downloads release v for this architecture, verifies the signature
// of SHA256SUMS and the hashes of the files, and replaces the guest kernel
// (if Kernel exists) and then the executable. Each file is renamed into
// place, so it is the old or the new file, never a partial one. The new
// binary runs after the process restarts; the caller exits so that systemd
// starts it again.
func (u *Updater) Apply(ctx context.Context, v string) error {
	if !version.IsRelease(v) {
		return fmt.Errorf("update: %q is not a release version", v)
	}
	pub, err := u.publicKey()
	if err != nil {
		return err
	}
	num := strings.TrimPrefix(v, "v")
	base := cmp.Or(u.DownloadURL, "https://github.com") + "/" + u.Repo + "/releases/download/" + v + "/"

	var sums, sig bytes.Buffer
	if err := u.download(ctx, base+"SHA256SUMS", &sums, maxMetaSize); err != nil {
		return err
	}
	if err := u.download(ctx, base+"SHA256SUMS.sig", &sig, maxMetaSize); err != nil {
		return err
	}
	if !ed25519.Verify(pub, sums.Bytes(), sig.Bytes()) {
		return errors.New("update: the signature of SHA256SUMS is not valid")
	}
	hashes := parseSums(sums.Bytes())
	name, err := findArchive(hashes, num, runtime.GOARCH)
	if err != nil {
		return err
	}
	// kernel/build.sh and install.sh name the kernel with the arch of uname -m.
	kernelName := "vmlinux-" + runtime.GOARCH
	if runtime.GOARCH == "amd64" {
		kernelName = "vmlinux-x86_64"
	}
	var kernel string // empty when this server has no guest kernel
	if _, err := os.Stat(u.Kernel); err == nil {
		kernel = u.Kernel
	}
	if kernel != "" && hashes[kernelName] == "" {
		return fmt.Errorf("update: SHA256SUMS has no entry for %s, the guest kernel of this server", kernelName)
	}

	archive, err := u.fetch(ctx, base+name, hashes[name], os.TempDir(), "runplane-update-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	exe, err := u.exe()
	if err != nil {
		return err
	}
	newExe, err := extract(exe, archive)
	if err != nil {
		return err
	}
	defer os.Remove(newExe)
	if kernel != "" {
		newKernel, err := u.fetch(ctx, base+kernelName, hashes[kernelName], filepath.Dir(kernel), ".vmlinux-update-*")
		if err != nil {
			return err
		}
		defer os.Remove(newKernel)
		if err := os.Chmod(newKernel, 0o644); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		if err := os.Rename(newKernel, kernel); err != nil {
			return fmt.Errorf("update: replace guest kernel: %w", err)
		}
	}
	if err := os.Rename(newExe, exe); err != nil {
		return fmt.Errorf("update: replace binary: %w", err)
	}
	return nil
}

// fetch downloads url to a new file in dir and checks its SHA-256 against
// want. It returns the path of the file.
func (u *Updater) fetch(ctx context.Context, url, want, dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	h := sha256.New()
	err = u.download(ctx, url, io.MultiWriter(f, h), maxFileSize)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if got := hex.EncodeToString(h.Sum(nil)); err == nil && got != want {
		err = fmt.Errorf("update: %s has SHA-256 %s, SHA256SUMS has %s", path.Base(url), got, want)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (u *Updater) publicKey() (ed25519.PublicKey, error) {
	if u.PublicKey == "" {
		return nil, errors.New("update: this build has no release signing key, so it cannot verify releases; install the new version with the install script")
	}
	key, err := base64.StdEncoding.DecodeString(u.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("update: the release signing key of this build is not a base64 ed25519 public key")
	}
	return key, nil
}

func (u *Updater) exe() (string, error) {
	exe := u.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", fmt.Errorf("update: find executable: %w", err)
		}
	}
	// Replace the file, not a symlink to it.
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("update: find executable: %w", err)
	}
	return real, nil
}

func (u *Updater) download(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	req.Header.Set("User-Agent", "runplane/"+u.Current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("update: download %s: %w", path.Base(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download %s: %s", path.Base(url), resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("update: download %s: %w", path.Base(url), err)
	}
	if n > limit {
		return fmt.Errorf("update: %s is larger than %d bytes", path.Base(url), limit)
	}
	return nil
}

// parseSums reads SHA256SUMS ("<hex>  <name>" lines, as sha256sum writes
// them) into hashes by file name.
func parseSums(sums []byte) map[string]string {
	hashes := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			hashes[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return hashes
}

// findArchive returns the name of the archive for this version and arch.
func findArchive(hashes map[string]string, num, arch string) (string, error) {
	names := []string{
		"runplane_" + num + "_linux_" + arch + ".tar.gz",
		"runplane_v" + num + "_linux_" + arch + ".tar.gz",
	}
	for _, name := range names {
		if _, ok := hashes[name]; ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("update: SHA256SUMS has no entry for %s", names[0])
}

// extract writes the runplane binary of the archive to a new file next to
// exe and returns its path. A rename then replaces exe atomically.
func extract(exe, archive string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("update: read archive: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("update: the archive has no runplane binary")
		}
		if err != nil {
			return "", fmt.Errorf("update: read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == "runplane" {
			return writeExe(exe, tr)
		}
	}
}

func writeExe(exe string, r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".runplane-update-*")
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	_, err = io.Copy(tmp, io.LimitReader(r, maxFileSize))
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("update: write binary: %w", err)
	}
	return tmp.Name(), nil
}
