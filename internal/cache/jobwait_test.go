package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// lateJob is a sandbox whose job runplane learns after linked is set, as
// with GitHub, which sends the job message after the job starts.
type lateJob struct{ linked atomic.Bool }

func (l *lateJob) Sandbox(context.Context, string, http.Header) (Identity, error) {
	if !l.linked.Load() {
		return Identity{SandboxID: "sb-1"}, nil
	}
	return acme("sb-1", "refs/heads/main", ""), nil
}

// The first cache step of a job runs before runplane knows the job of the
// sandbox. The request must wait for the job, not fail with 403.
func TestCacheRequestWaitsForTheJob(t *testing.T) {
	tests := []struct {
		name    string
		linkIn  time.Duration // 0 = never
		wantErr error
	}{
		{"job known after 200 ms", 200 * time.Millisecond, nil},
		{"job never known", 0, ErrNoSandbox},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &lateJob{}
			s, err := New(Config{Dir: t.TempDir(), Index: NewMemIndex(), Resolver: r, JobWait: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if tt.linkIn > 0 {
				time.AfterFunc(tt.linkIn, func() { r.linked.Store(true) })
			}
			ident, err := s.identify(httptest.NewRequest("POST", "/twirp/x", nil), false)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && ident.Repository != "acme/app" {
				t.Errorf("identity %+v, want the repository of the job", ident)
			}
		})
	}
}
