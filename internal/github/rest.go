package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/version"
)

// APIError is a GitHub REST response with an error status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
	// RetryAt is set when GitHub rate-limits the request and the wait is
	// longer than runplane accepts in one call.
	RetryAt time.Time
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("GitHub API %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if !e.RetryAt.IsZero() {
		msg += fmt.Sprintf(" (rate limit, try again after %s)", e.RetryAt.UTC().Format(time.RFC3339))
	}
	return msg
}

func isStatus(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
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
	timeout    time.Duration // for one attempt, the body read included
	maxWait    time.Duration // longest rate limit wait in one call
	retryWait  time.Duration // first wait after a network error or a 5xx
}

func newREST(hc *http.Client, base string) *rest {
	nr := *hc
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &rest{
		http:       hc,
		noRedirect: &nr,
		base:       strings.TrimRight(base, "/"),
		timeout:    30 * time.Second,
		maxWait:    time.Minute,
		retryWait:  time.Second,
	}
}

// call is one REST request. path is relative to the API URL or absolute
// (pagination links).
type call struct {
	method     string
	path       string
	auth       auth // nil = anonymous
	body       any
	timeout    time.Duration // 0 = rest.timeout
	noRedirect bool
}

// do sends the call and returns a response with a status below 400. The
// caller closes the body; closing it also ends the attempt's timeout. do
// tries again after up to three short rate limit waits, after network errors
// and 5xx answers of safe methods, and once after a refused cached token.
func (c *rest) do(ctx context.Context, cl call) (*http.Response, error) {
	var body []byte
	if cl.body != nil {
		var err error
		if body, err = json.Marshal(cl.body); err != nil {
			return nil, fmt.Errorf("encode %s %s: %w", cl.method, cl.path, err)
		}
	}
	timeout := cmp.Or(cl.timeout, c.timeout)
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
		resp, cancel, err := c.send(ctx, cl, authorization, body, timeout)
		if err != nil {
			if ctx.Err() != nil || !safe || failures >= 2 {
				return nil, err
			}
			failures++
			if werr := sleep(ctx, c.retryWait<<(failures-1)); werr != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 400 {
			resp.Body = cancelOnClose{resp.Body, cancel}
			return resp, nil
		}

		apiErr := readAPIError(cl.method, cl.path, resp)
		cancel()
		switch wait, limited := rateLimitWait(resp, apiErr.Message, time.Now()); {
		case limited && wait <= c.maxWait && limits < 3:
			limits++
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		case limited:
			apiErr.RetryAt = time.Now().Add(wait)
			return nil, apiErr
		case resp.StatusCode == http.StatusUnauthorized && cl.auth != nil && !refreshed:
			cl.auth.invalidate()
			refreshed = true
			continue
		case resp.StatusCode >= 500 && safe && failures < 2:
			failures++
			if err := sleep(ctx, c.retryWait<<(failures-1)); err != nil {
				return nil, apiErr
			}
			continue
		}
		return nil, apiErr
	}
}

func (c *rest) send(ctx context.Context, cl call, authorization string, body []byte, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	u := cl.path
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = c.base + u
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(actx, cl.method, u, rd)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("make request %s %s: %w", cl.method, cl.path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "runplane/"+version.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
		return nil, nil, fmt.Errorf("GitHub API %s %s: %w", cl.method, cl.path, err)
	}
	return resp, cancel, nil
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
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(target), "<>")
	}
	return ""
}

func readAPIError(method, path string, resp *http.Response) *APIError {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode}
	if u, err := url.Parse(path); err == nil && u.IsAbs() {
		e.Path = u.Path
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &body) == nil {
		e.Message = body.Message
	} else {
		e.Message = strings.TrimSpace(string(b))
	}
	return e
}

// rateLimitWait tells if a response is a rate limit and how long to wait.
// GitHub sends Retry-After for secondary limits and X-RateLimit-Reset for
// the primary limit. Without either, GitHub asks for at least one minute.
func rateLimitWait(resp *http.Response, message string, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(reset, 0).Sub(now)+time.Second, 0), true
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
