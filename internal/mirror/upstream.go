package mirror

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// upstream sends registry requests to Docker Hub. It gets a bearer token for
// each repository with the token flow of the registry and keeps the token
// until it expires.
type upstream struct {
	base   string // for example https://registry-1.docker.io
	client *http.Client

	mu     sync.Mutex
	tokens map[string]bearer // by repository name
}

type bearer struct {
	token   string
	expires time.Time
}

// upstreamError is a status from upstream that is not a success.
type upstreamError struct{ status int }

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream status %d %s", e.status, http.StatusText(e.status))
}

// do sends method for /v2/<name>/<path>. If upstream asks for a token, do gets
// one for pull access to name and sends the request again.
func (u *upstream) do(ctx context.Context, method, name, path, accept string) (*http.Response, error) {
	send := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u.base+"/v2/"+name+"/"+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		// Blobs are compressed already. Without this header, the transport
		// can ask for gzip and then hide the length of the blob.
		req.Header.Set("Accept-Encoding", "identity")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return u.client.Do(req) // a *url.Error has the method and the URL
	}

	resp, err := send(u.cachedToken(name))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	drain(resp)
	token, err := u.fetchToken(ctx, challenge, name)
	if err != nil {
		return nil, err
	}
	return send(token)
}

func (u *upstream) cachedToken(name string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if b, ok := u.tokens[name]; ok && time.Now().Before(b.expires) {
		return b.token
	}
	return ""
}

// fetchToken gets a pull token for name from the realm of the Bearer
// challenge.
func (u *upstream) fetchToken(ctx context.Context, challenge, name string) (string, error) {
	params, ok := parseChallenge(challenge)
	if !ok || params["realm"] == "" {
		return "", fmt.Errorf("get token: unsupported auth challenge %q", challenge)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return "", fmt.Errorf("get token: bad realm %q", params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+name+":pull")
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get token: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	token := cmp.Or(body.Token, body.AccessToken)
	if token == "" {
		return "", fmt.Errorf("get token: empty token")
	}
	// The token spec gives 60 seconds when expires_in is missing. Renew a
	// bit early, so that a token does not expire during a request.
	ttl := time.Duration(cmp.Or(body.ExpiresIn, 60))*time.Second - 10*time.Second
	u.mu.Lock()
	if u.tokens == nil {
		u.tokens = map[string]bearer{}
	}
	u.tokens[name] = bearer{token, time.Now().Add(ttl)}
	u.mu.Unlock()
	return token, nil
}

// parseChallenge reads the parameters of a WWW-Authenticate header such as
// `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`.
func parseChallenge(h string) (map[string]string, bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimLeft(rest, ", ") {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, false
		}
		var val string
		if quoted, ok := strings.CutPrefix(after, `"`); ok {
			end := strings.IndexByte(quoted, '"')
			if end < 0 {
				return nil, false
			}
			val, rest = quoted[:end], quoted[end+1:]
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(val)
	}
	return params, true
}

// drain reads a little of the body so that the connection can serve the next
// request, then closes it.
func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
