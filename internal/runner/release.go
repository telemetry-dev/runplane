package runner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/blob"
	"github.com/runplane/runplane/internal/ghrest"
)

// arches are the runner arches of the servers.
var arches = []string{"x64", "arm64"}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Release is a release of actions/runner.
type Release struct {
	Version     string // for example "2.330.0"
	PublishedAt *time.Time
	// SHA256 has the hex SHA-256 of the linux archive of each arch.
	SHA256 map[string]string
}

// Releases reads the releases of actions/runner on GitHub. The zero value
// reads github.com without a token.
type Releases struct {
	API  ghrest.Client
	Web  string      // the GitHub URL of the archives; ghrest.Web if empty
	Auth ghrest.Auth // nil for anonymous reads, which have a lower rate limit
}

// Latest reads the latest release.
func (r Releases) Latest(ctx context.Context) (Release, error) {
	return r.read(ctx, "latest")
}

// Get reads the release of version.
func (r Releases) Get(ctx context.Context, version string) (Release, error) {
	return r.read(ctx, "tags/v"+version)
}

func (r Releases) read(ctx context.Context, path string) (Release, error) {
	var body struct {
		TagName     string     `json:"tag_name"`
		PublishedAt *time.Time `json:"published_at"`
		Body        string     `json:"body"`
		Assets      []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	call := ghrest.Call{Method: http.MethodGet, Path: "/repos/actions/runner/releases/" + path, Auth: r.Auth}
	if err := r.API.JSON(ctx, call, &body); err != nil {
		return Release{}, err
	}
	rel := Release{Version: strings.TrimPrefix(body.TagName, "v"), PublishedAt: body.PublishedAt, SHA256: map[string]string{}}
	if !versionRE.MatchString(rel.Version) {
		return Release{}, fmt.Errorf("runner release %s has the tag %q, not a version", path, body.TagName)
	}
	for _, arch := range arches {
		sum := markedSHA(body.Body, "linux-"+arch)
		if sum == "" {
			// GitHub also gives the digest of each asset.
			for _, a := range body.Assets {
				if a.Name == archiveName(rel.Version, arch) {
					sum, _ = strings.CutPrefix(a.Digest, "sha256:")
				}
			}
		}
		if blob.ValidDigest("sha256:" + sum) {
			rel.SHA256[arch] = sum
		}
	}
	return rel, nil
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

func archiveName(version, arch string) string {
	return "actions-runner-linux-" + arch + "-" + version + ".tar.gz"
}

// download writes the archive of a release to dst and returns its SHA-256
// in hex.
func (r Releases) download(ctx context.Context, version, arch, dst string) (string, error) {
	url := cmp.Or(r.Web, ghrest.Web) + "/actions/runner/releases/download/v" + version + "/" + archiveName(version, arch)
	resp, err := r.API.Download(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
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

// TokenAuth sends the token that token gives, if it gives one. It returns
// nil for a nil token.
func TokenAuth(token func(context.Context) (string, error)) ghrest.Auth {
	if token == nil {
		return nil
	}
	return tokenAuth(token)
}

type tokenAuth func(context.Context) (string, error)

func (t tokenAuth) Header(ctx context.Context) (string, error) {
	token, err := t(ctx)
	if err != nil {
		return "", fmt.Errorf("get GitHub token: %w", err)
	}
	if token == "" {
		return "", nil
	}
	return "Bearer " + token, nil
}
