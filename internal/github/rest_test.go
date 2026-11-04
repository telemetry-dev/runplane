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

func TestRESTRateLimits(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	tests := []struct {
		name      string
		limited   func(w http.ResponseWriter)
		wantCalls int
		wantErr   bool
		minWait   time.Duration
	}{
		{
			name: "secondary limit with Retry-After waits and tries again",
			limited: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "You have exceeded a secondary rate limit."})
			},
			wantCalls: 2, minWait: time.Second,
		},
		{
			name: "429 with Retry-After waits and tries again",
			limited: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantCalls: 2, minWait: time.Second,
		},
		{
			name: "primary limit with a far reset fails at once",
			limited: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "API rate limit exceeded"})
			},
			wantCalls: 1, wantErr: true,
		},
		{
			name: "403 without rate limit signs is no rate limit",
			limited: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
			},
			wantCalls: 1, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					tt.limited(w)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			}))
			defer srv.Close()
			c := newREST(srv.Client(), srv.URL)
			start := time.Now()
			var out struct{ OK bool }
			err := c.json(context.Background(), call{method: http.MethodGet, path: "/x"}, &out)
			if calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr && !out.OK {
				t.Error("the answer of the second try is lost")
			}
			if waited := time.Since(start); waited < tt.minWait {
				t.Errorf("waited %s, want at least %s", waited, tt.minWait)
			}
		})
	}
}

func TestRESTPrimaryLimitReportsReset(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := newREST(srv.Client(), srv.URL).json(context.Background(), call{method: http.MethodGet, path: "/x"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if d := apiErr.RetryAt.Sub(reset); d < 0 || d > 5*time.Second {
		t.Errorf("RetryAt = %s, want about the reset time %s", apiErr.RetryAt, reset)
	}
}

func TestRESTRetriesServerErrorsOnlyForReads(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		c := newREST(srv.Client(), srv.URL)
		c.retryWait = time.Millisecond
		err := c.json(context.Background(), call{method: method, path: "/x"}, nil)
		srv.Close()
		if method == http.MethodGet && (err != nil || calls != 2) {
			t.Errorf("GET: err = %v, calls = %d; want a second try", err, calls)
		}
		// A POST such as a re-run must not happen twice.
		if method == http.MethodPost && (err == nil || calls != 1) {
			t.Errorf("POST: err = %v, calls = %d; want one call and the error", err, calls)
		}
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
