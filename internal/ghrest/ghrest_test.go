package ghrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestRetries(t *testing.T) {
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
			c := Client{URL: srv.URL, RetryWait: time.Millisecond}
			start := time.Now()
			var out struct{ OK bool }
			err := c.JSON(context.Background(), Call{Method: tt.method, Path: "/x"}, &out)
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
			var apiErr *APIError
			var retryAt time.Time
			if errors.As(err, &apiErr) {
				retryAt = apiErr.RetryAt
			}
			if d := retryAt.Sub(tt.wantRetry); d < 0 || d > 5*time.Second {
				t.Errorf("RetryAt = %s, want about %s", retryAt, tt.wantRetry)
			}
		})
	}
}

// GitHub limits the anonymous calls of an IP address, so after one
// anonymous call gets a long limit, the others wait for its end too.
func TestAnonymousCallsShareTheRateLimit(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "API rate limit exceeded"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := (Client{URL: srv.URL}).JSON(ctx, Call{Method: http.MethodGet, Path: "/first"}, nil); err == nil {
		t.Fatal("the first anonymous call has no error")
	}

	err := (Client{URL: srv.URL}).JSON(ctx, Call{Method: http.MethodGet, Path: "/second"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Path != "/second" {
		t.Fatalf("second anonymous call: %v, want a rate limit error", err)
	}
	if d := apiErr.RetryAt.Sub(reset); d < 0 || d > 5*time.Second {
		t.Errorf("RetryAt = %s, want about %s", apiErr.RetryAt, reset)
	}
	if calls["/second"] != 0 {
		t.Errorf("the second anonymous call went to GitHub")
	}
	if err := (Client{URL: srv.URL}).JSON(ctx, Call{Method: http.MethodGet, Path: "/token", Auth: token("ghs_x")}, nil); err != nil {
		t.Errorf("call with a token: %v", err)
	}
}

type token string

func (t token) Header(context.Context) (string, error) { return "Bearer " + string(t), nil }

// List follows the rel="next" link of each page, and the Link header of
// GitHub also has the prev, first, and last pages.
func TestList(t *testing.T) {
	var srv *httptest.Server
	calls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := func(n int) string { return "<" + srv.URL + "/repos?per_page=2&page=" + strconv.Itoa(n) + ">" }
		if calls > 3 { // a List that does not stop
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", page(2)+`; rel="next", `+page(3)+`; rel="last"`)
			writeJSON(w, http.StatusOK, map[string]any{"total_count": 5, "items": []string{"a", "b"}})
		case "2":
			w.Header().Set("Link", page(1)+`; rel="prev", `+page(3)+`; rel="next", `+page(3)+`; rel="last", `+page(1)+`; rel="first"`)
			writeJSON(w, http.StatusOK, map[string]any{"total_count": 5, "items": []string{"c", "d"}})
		case "3":
			w.Header().Set("Link", page(2)+`; rel="prev", `+page(1)+`; rel="first"`)
			writeJSON(w, http.StatusOK, map[string]any{"total_count": 5, "items": []string{"e"}})
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		}
	}))
	defer srv.Close()
	got, err := List[string](context.Background(), Client{URL: srv.URL}, token("ghs_x"), "/repos?per_page=2", "items")
	if err != nil || !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) || calls != 3 {
		t.Errorf("List = %q, %v after %d calls; want a to e from 3 pages", got, err, calls)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
