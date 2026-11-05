package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/version"
)

// apiError is a GitHub REST response with an error status.
type apiError struct {
	method, path string
	status       int
	message      string
	// retryAt is set when GitHub rate-limits the request and the wait is
	// longer than runplane accepts in one call.
	retryAt time.Time
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("GitHub API %s %s: %d %s", e.method, e.path, e.status, http.StatusText(e.status))
	if e.message != "" {
		msg += ": " + e.message
	}
	if !e.retryAt.IsZero() {
		msg += fmt.Sprintf(" (rate limit, try again after %s)", e.retryAt.UTC().Format(time.RFC3339))
	}
	return msg
}

// auth gives the Authorization header of a request.
type auth interface {
	header(ctx context.Context) (string, error)
	// invalidate drops a cached credential after GitHub refused it.
	invalidate()
}

// rest is a small client for the GitHub REST API.
type rest struct {
	http       *http.Client
	noRedirect *http.Client
	base       string        // API URL without a trailing slash
	retryWait  time.Duration // first wait after a network error or a 5xx
}

const (
	callTimeout = 30 * time.Second // for one attempt, the body read included
	maxRateWait = time.Minute      // longest rate limit wait in one call
)

func newREST(base string) *rest {
	return &rest{
		http:       &http.Client{},
		noRedirect: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		base:       base,
		retryWait:  time.Second,
	}
}

// call is one REST request. path is relative to the API URL or absolute
// (pagination links).
type call struct {
	method     string
	path       string
	auth       auth          // nil = anonymous
	timeout    time.Duration // 0 = callTimeout
	noRedirect bool
}

// do sends the call and returns a response with a status below 400. The
// caller closes the body; closing it also ends the attempt's timeout. do
// tries again after up to three short rate limit waits, after network errors
// and 5xx answers of safe methods, and once after a refused cached token.
func (c *rest) do(ctx context.Context, cl call) (*http.Response, error) {
	safe := cl.method == http.MethodGet || cl.method == http.MethodHead
	refreshed := false
	failures, limits := 0, 0
	for {
		var authorization string
		if cl.auth != nil {
			var err error
			if authorization, err = cl.auth.header(ctx); err != nil {
				return nil, err
			}
		}
		resp, err := c.send(ctx, cl, authorization)
		if err == nil {
			if resp.StatusCode < 400 {
				return resp, nil
			}
			apiErr := readAPIError(cl.method, cl.path, resp)
			switch wait, limited := rateLimitWait(resp, apiErr.message); {
			case limited && wait <= maxRateWait && limits < 3:
				limits++
				if err := sleep(ctx, wait); err != nil {
					return nil, err
				}
				continue
			case limited:
				apiErr.retryAt = time.Now().Add(wait)
				return nil, apiErr
			case resp.StatusCode == http.StatusUnauthorized && cl.auth != nil && !refreshed:
				cl.auth.invalidate()
				refreshed = true
				continue
			case resp.StatusCode < 500:
				return nil, apiErr
			}
			err = apiErr
		}
		if ctx.Err() != nil || !safe || failures >= 2 {
			return nil, err
		}
		failures++
		if sleep(ctx, c.retryWait<<(failures-1)) != nil {
			return nil, err
		}
	}
}

// send sends one attempt. Closing the response body ends its timeout.
func (c *rest) send(ctx context.Context, cl call, authorization string) (*http.Response, error) {
	u := cl.path
	if strings.HasPrefix(u, "/") {
		u = c.base + u
	}
	actx, cancel := context.WithTimeout(ctx, cmp.Or(cl.timeout, callTimeout))
	req, err := http.NewRequestWithContext(actx, cl.method, u, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("make request %s %s: %w", cl.method, cl.path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "runplane/"+version.Version)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	hc := c.http
	if cl.noRedirect {
		hc = c.noRedirect
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("GitHub API %s %s: %w", cl.method, cl.path, err)
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

// json sends the call and decodes a JSON answer into out (if not nil).
func (c *rest) json(ctx context.Context, cl call, out any) error {
	resp, err := c.do(ctx, cl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode GitHub API %s %s: %w", cl.method, cl.path, err)
	}
	return nil
}

// list reads all pages of a GET list. key is the field of the page object
// that holds the items, or "" when each page is a JSON array. path must
// have per_page in its query.
func list[T any](ctx context.Context, c *rest, a auth, path, key string) ([]T, error) {
	var all []T
	for next := path; next != ""; {
		resp, err := c.do(ctx, call{method: http.MethodGet, path: next, auth: a})
		if err != nil {
			return nil, err
		}
		var page []T
		err = decodePage(resp.Body, key, &page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode GitHub API GET %s: %w", next, err)
		}
		all = append(all, page...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

func decodePage(r io.Reader, key string, out any) error {
	if key == "" {
		return json.NewDecoder(r).Decode(out)
	}
	var obj map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&obj); err != nil {
		return err
	}
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// nextLink returns the rel="next" URL of a Link header.
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

func readAPIError(method, path string, resp *http.Response) *apiError {
	e := &apiError{method: method, path: path, status: resp.StatusCode}
	if u, err := url.Parse(path); err == nil && u.IsAbs() {
		e.path = u.Path
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &body) == nil {
		e.message = body.Message
	} else {
		e.message = strings.TrimSpace(string(b))
	}
	return e
}

// rateLimitWait tells if a response is a rate limit and how long to wait.
// GitHub sends Retry-After for secondary limits and X-RateLimit-Reset for
// the primary limit. Without either, GitHub asks for at least one minute.
func rateLimitWait(resp *http.Response, message string) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Until(time.Unix(reset, 0))+time.Second, 0), true
		}
		return time.Minute, true
	}
	if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(message), "secondary rate limit") {
		return time.Minute, true
	}
	return 0, false
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
