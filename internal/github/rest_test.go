package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestRESTRetries(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	tests := []struct {
		name      string
		method    string
		first     func(w http.ResponseWriter)
		wantCalls int
		wantWait  time.Duration
		wantRetry time.Time // RetryAt of the error, zero for no error
	}{
		{
			name: "secondary limit with Retry-After waits and tries again", method: http.MethodGet,
			first: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "You have exceeded a secondary rate limit."})
			},
			wantCalls: 2, wantWait: time.Second,
		},
		{
			name: "429 with Retry-After waits and tries again", method: http.MethodPost,
			first: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantCalls: 2, wantWait: time.Second,
		},
		{
			name: "primary limit with a far reset fails at once", method: http.MethodGet,
			first: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "API rate limit exceeded"})
			},
			wantCalls: 1, wantRetry: reset,
		},
		{
			name: "403 without rate limit signs is no rate limit", method: http.MethodGet,
			first: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
			},
			wantCalls: 1,
		},
		{
			name: "5xx of a read tries again", method: http.MethodGet,
			first:     func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			wantCalls: 2,
		},
		{
			// A POST such as a re-run must not happen twice.
			name: "5xx of a write fails at once", method: http.MethodPost,
			first:     func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					tt.first(w)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			}))
			defer srv.Close()
			c := newREST(srv.URL)
			c.retryWait = time.Millisecond
			start := time.Now()
			var out struct{ OK bool }
			err := c.json(context.Background(), call{method: tt.method, path: "/x"}, &out)
			if calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
			// Only the second try gets an answer.
			if (err == nil) != (tt.wantCalls == 2) || err == nil && !out.OK {
				t.Errorf("err = %v, answer = %v", err, out.OK)
			}
			if waited := time.Since(start); waited < tt.wantWait {
				t.Errorf("waited %s, want at least %s", waited, tt.wantWait)
			}
			var apiErr *apiError
			var retryAt time.Time
			if errors.As(err, &apiErr) {
				retryAt = apiErr.retryAt
			}
			if d := retryAt.Sub(tt.wantRetry); d < 0 || d > 5*time.Second {
				t.Errorf("RetryAt = %s, want about %s", retryAt, tt.wantRetry)
			}
		})
	}
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/app/installations?page=1>; rel="prev", <https://api.github.com/app/installations?page=3>; rel="next", <https://api.github.com/app/installations?page=5>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/app/installations?page=3" {
		t.Errorf("nextLink = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="first"`); got != "" {
		t.Errorf("nextLink on the last page = %q, want empty", got)
	}
}
