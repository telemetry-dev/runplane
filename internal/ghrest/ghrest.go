// Package ghrest calls the GitHub REST API and downloads files from GitHub.
// It sends the headers that GitHub asks for, tries again after network
// errors and rate limits, and reads all pages of a list. It also gives the
// GitHub account types and the web pages of an account.
package ghrest

import (
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
	"sync"
	"time"

	"github.com/runplane/runplane/internal/version"
)

const (
	API = "https://api.github.com"
	Web = "https://github.com"
)

const (
	callTimeout = 30 * time.Second // for one attempt, the body read included
	maxRateWait = time.Minute      // longest rate limit wait in one call
)

// Client calls GitHub. The zero value calls api.github.com.
type Client struct {
	URL string // the API URL without a trailing slash; API if empty
	// RetryWait is the first wait after a network error or a 5xx answer;
	// one second if zero.
	RetryWait time.Duration
}

// Call is one REST request. Path is relative to the API URL, or absolute.
type Call struct {
	Method     string
	Path       string
	Auth       Auth          // nil for an anonymous call
	Timeout    time.Duration // of one attempt with its body; 0 for 30 seconds
	NoRedirect bool          // Do returns a redirect and does not follow it

	download bool
}

// Auth gives the Authorization header of a call. An empty header makes the
// call anonymous.
type Auth interface {
	Header(ctx context.Context) (string, error)
}

// Renewable is an Auth with a cached credential. After GitHub refuses the
// credential, Do calls Invalidate and tries once more.
type Renewable interface {
	Auth
	Invalidate()
}

// APIError is a GitHub answer with an error status. A call that the
// anonymous rate limit stops gets a copy of the answer that set the limit.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
	// RetryAt is set when GitHub rate-limits the call and the wait is
	// longer than runplane accepts in one call.
	RetryAt time.Time
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("GitHub %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if !e.RetryAt.IsZero() {
		msg += fmt.Sprintf(" (rate limit, try again after %s)", e.RetryAt.UTC().Format(time.RFC3339))
	}
	return msg
}

// Do sends the call and returns a response with a status below 400. The
// caller closes the body; that also ends the timeout of the attempt. Only a
// GET or a HEAD is sent again after a network error or a 5xx answer.
func (c Client) Do(ctx context.Context, cl Call) (*http.Response, error) {
	safe := cl.Method == http.MethodGet || cl.Method == http.MethodHead
	renew, _ := cl.Auth.(Renewable)
	failures, limits := 0, 0
	for {
		var authorization string
		if cl.Auth != nil {
			var err error
			if authorization, err = cl.Auth.Header(ctx); err != nil {
				return nil, err
			}
		}
		var shared *budget
		if authorization == "" && !cl.download {
			shared = anonymous(c.url())
			if err := shared.check(cl); err != nil {
				return nil, err
			}
		}
		resp, err := c.send(ctx, cl, authorization)
		if err == nil {
			if resp.StatusCode < 400 {
				return resp, nil
			}
			apiErr := readAPIError(cl, resp)
			switch wait, limited := rateLimitWait(resp, apiErr.Message); {
			case limited && wait <= maxRateWait && limits < 3:
				limits++
				if err := sleep(ctx, wait); err != nil {
					return nil, err
				}
				continue
			case limited:
				apiErr.RetryAt = time.Now().Add(wait)
				if shared != nil {
					shared.hold(*apiErr)
				}
				return nil, apiErr
			case resp.StatusCode == http.StatusUnauthorized && renew != nil:
				renew.Invalidate()
				renew = nil
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
		if sleep(ctx, cmp.Or(c.RetryWait, time.Second)<<(failures-1)) != nil {
			return nil, err
		}
	}
}

// JSON sends the call and decodes a JSON answer into out (if not nil).
func (c Client) JSON(ctx context.Context, cl Call, out any) error {
	resp, err := c.Do(ctx, cl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode GitHub %s %s: %w", cl.Method, callPath(cl.Path), err)
	}
	return nil
}

// List reads all pages of a GET list. key is the field of the page object
// that holds the items, or "" when each page is a JSON array. path must
// have per_page in its query.
func List[T any](ctx context.Context, c Client, a Auth, path, key string) ([]T, error) {
	var all []T
	for next := path; next != ""; {
		resp, err := c.Do(ctx, Call{Method: http.MethodGet, Path: next, Auth: a})
		if err != nil {
			return nil, err
		}
		var page []T
		err = decodePage(resp.Body, key, &page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode GitHub GET %s: %w", callPath(next), err)
		}
		all = append(all, page...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// Download sends a GET for a file outside the API, such as a release asset,
// without the API headers and a token. It follows redirects and tries again
// as Do does. Only ctx limits the time. The caller closes the body.
func (c Client) Download(ctx context.Context, url string) (*http.Response, error) {
	return c.Do(ctx, Call{Method: http.MethodGet, Path: url, download: true})
}

func (c Client) url() string { return cmp.Or(c.URL, API) }

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (c Client) send(ctx context.Context, cl Call, authorization string) (*http.Response, error) {
	u := cl.Path
	if strings.HasPrefix(u, "/") {
		u = c.url() + u
	}
	var actx context.Context
	var cancel context.CancelFunc
	if cl.download {
		actx, cancel = context.WithCancel(ctx)
	} else {
		actx, cancel = context.WithTimeout(ctx, cmp.Or(cl.Timeout, callTimeout))
	}
	req, err := http.NewRequestWithContext(actx, cl.Method, u, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("make request %s %s: %w", cl.Method, callPath(cl.Path), err)
	}
	req.Header.Set("User-Agent", "runplane/"+version.Version)
	if !cl.download {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	hc := http.DefaultClient
	if cl.NoRedirect {
		hc = noRedirect
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		// The url.Error has the full URL, and a signed URL must not go
		// to the logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("GitHub %s %s: %w", cl.Method, callPath(cl.Path), err)
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

// callPath is the path of a call without the host and the query.
func callPath(p string) string {
	if u, err := url.Parse(p); err == nil {
		return u.Path
	}
	return p
}

func readAPIError(cl Call, resp *http.Response) *APIError {
	e := &APIError{Method: cl.Method, Path: callPath(cl.Path), Status: resp.StatusCode}
	defer resp.Body.Close()
	if cl.download {
		return e
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
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

// budget is the rate limit of the anonymous calls to one API URL. GitHub
// counts these calls by IP address, so all clients of the process share it:
// after GitHub limits one call for longer than a call waits, the other
// calls fail at once until the limit ends.
type budget struct {
	mu   sync.Mutex
	held APIError // the answer that limits the calls until its RetryAt
}

var budgets sync.Map // of *budget by API URL

func anonymous(api string) *budget {
	b, _ := budgets.LoadOrStore(api, &budget{})
	return b.(*budget)
}

func (b *budget) check(cl Call) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !time.Now().Before(b.held.RetryAt) {
		return nil
	}
	e := b.held
	e.Method, e.Path = cl.Method, callPath(cl.Path)
	return &e
}

func (b *budget) hold(e APIError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held = e
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
