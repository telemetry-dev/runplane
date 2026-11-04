package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const keysTTL = 10 * time.Minute

// GitHubKeys reads the public SSH keys of GitHub users from
// https://github.com/<login>.keys and keeps each answer for 10 minutes.
type GitHubKeys struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cachedKeys // by lowercase login
}

type cachedKeys struct {
	keys    []ssh.PublicKey
	expires time.Time
}

// NewGitHubKeys returns a key reader that uses client, or a client with a
// 10-second timeout if client is nil.
func NewGitHubKeys(client *http.Client) *GitHubKeys {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &GitHubKeys{client: client, cache: map[string]cachedKeys{}}
}

// githubLogin matches GitHub user names. Bot actors such as
// "dependabot[bot]" do not match and have no keys.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// Keys returns the keys of the user. An unknown user has no keys. A failed
// read is not kept, so the next login tries again.
func (g *GitHubKeys) Keys(ctx context.Context, login string) ([]ssh.PublicKey, error) {
	if !githubLogin.MatchString(login) {
		return nil, nil
	}
	login = strings.ToLower(login)
	now := time.Now()
	g.mu.Lock()
	e, ok := g.cache[login]
	g.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.keys, nil
	}

	keys, err := g.fetch(ctx, login)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for l, e := range g.cache {
		if !now.Before(e.expires) {
			delete(g.cache, l)
		}
	}
	g.cache[login] = cachedKeys{keys: keys, expires: now.Add(keysTTL)}
	return keys, nil
}

func (g *GitHubKeys) fetch(ctx context.Context, login string) ([]ssh.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+login+".keys", nil)
	if err != nil {
		return nil, fmt.Errorf("read github keys: %w", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read github keys: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("read github keys of %s: GitHub answered %s", login, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read github keys: %w", err)
	}
	return parseKeys(data), nil
}

// AuthorizedKeysFile returns a func for Config.AdminKeys. The func reads the
// file at path, in the authorized_keys format, at each login, so a change
// needs no restart. A missing file means no admin keys. Key options such as
// from= have no effect.
func AuthorizedKeysFile(path string, log *slog.Logger) func(context.Context) []ssh.PublicKey {
	return func(context.Context) []ssh.PublicKey {
		data, err := os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				log.Warn("read admin ssh keys", "path", path, "err", err)
			}
			return nil
		}
		return parseKeys(data)
	}
}

// parseKeys reads the keys of an authorized_keys file and skips bad lines.
func parseKeys(data []byte) []ssh.PublicKey {
	var keys []ssh.PublicKey
	for len(data) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break // no more keys
		}
		keys = append(keys, key)
		data = rest
	}
	return keys
}

func hasKey(keys []ssh.PublicKey, key ssh.PublicKey) bool {
	want := key.Marshal()
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), want) {
			return true
		}
	}
	return false
}
