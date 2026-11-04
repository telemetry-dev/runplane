package shell

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeGitHub answers in place of github.com and records the request URLs.
type fakeGitHub struct {
	status int
	body   string
	urls   []string
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	f.urls = append(f.urls, r.URL.String())
	return &http.Response{
		StatusCode: f.status,
		Status:     http.StatusText(f.status),
		Header:     http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    r,
	}, nil
}

func authorizedLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestGitHubKeys(t *testing.T) {
	a, b := newSigner(t).PublicKey(), newSigner(t).PublicKey()
	// github.com/<login>.keys gives one key on each line, without comments.
	gh := &fakeGitHub{status: http.StatusOK, body: authorizedLine(a) + "\nnot a key\n" + authorizedLine(b) + "\n"}
	keys := NewGitHubKeys(&http.Client{Transport: gh})

	got, err := keys.Keys(context.Background(), "OctoCat")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !hasKey(got, a) || !hasKey(got, b) {
		t.Errorf("Keys returned %d keys, want the 2 keys of the user", len(got))
	}
	if _, err := keys.Keys(context.Background(), "octocat"); err != nil {
		t.Fatal(err)
	}
	if len(gh.urls) != 1 || gh.urls[0] != "https://github.com/octocat.keys" {
		t.Errorf("requests %q, want one request to https://github.com/octocat.keys for both calls", gh.urls)
	}
}

func TestGitHubKeysUnknownUser(t *testing.T) {
	gh := &fakeGitHub{status: http.StatusNotFound, body: "Not Found"}
	got, err := NewGitHubKeys(&http.Client{Transport: gh}).Keys(context.Background(), "nobody-here")
	if err != nil || len(got) != 0 {
		t.Errorf("Keys = %d keys, %v; want no keys and no error", len(got), err)
	}
}

func TestGitHubKeysFailureIsNotKept(t *testing.T) {
	gh := &fakeGitHub{status: http.StatusServiceUnavailable}
	keys := NewGitHubKeys(&http.Client{Transport: gh})
	if _, err := keys.Keys(context.Background(), "octocat"); err == nil {
		t.Fatal("Keys = nil error for a 503 answer")
	}
	k := newSigner(t).PublicKey()
	gh.status, gh.body = http.StatusOK, authorizedLine(k)
	got, err := keys.Keys(context.Background(), "octocat")
	if err != nil || !hasKey(got, k) {
		t.Errorf("Keys after GitHub is back = %d keys, %v; want the key", len(got), err)
	}
}

func TestGitHubKeysBotHasNoKeys(t *testing.T) {
	gh := &fakeGitHub{status: http.StatusOK}
	got, err := NewGitHubKeys(&http.Client{Transport: gh}).Keys(context.Background(), "dependabot[bot]")
	if err != nil || len(got) != 0 || len(gh.urls) != 0 {
		t.Errorf("Keys = %d keys, %v, requests %q; want nothing", len(got), err, gh.urls)
	}
}

func TestAuthorizedKeysFileReadsChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	adminKeys := AuthorizedKeysFile(path, slog.New(slog.DiscardHandler))
	if got := adminKeys(context.Background()); len(got) != 0 {
		t.Errorf("missing file gave %d keys", len(got))
	}
	k := newSigner(t).PublicKey()
	if err := os.WriteFile(path, []byte("# ops\nno-pty "+authorizedLine(k)+" ops@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := adminKeys(context.Background()); len(got) != 1 || !hasKey(got, k) {
		t.Errorf("file with one key gave %d keys, want that key without a restart", len(got))
	}
}
