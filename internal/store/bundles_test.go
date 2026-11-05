package store

import (
	"errors"
	"testing"
	"time"
)

func TestLatestReadyBundle(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, b := range []RunnerBundle{
		{Version: "2.99.0", Arch: "x64", Status: BundleReady},
		{Version: "2.330.0", Arch: "x64", Status: BundleReady},
		{Version: "2.331.0", Arch: "x64", Status: BundleFailed},
		{Version: "2.340.0", Arch: "arm64", Status: BundleReady},
	} {
		b.CreatedAt = time.Now()
		must(t, s.UpsertRunnerBundle(ctx, b))
	}
	b, err := s.LatestReadyBundle(ctx, "x64")
	must(t, err)
	if b.Version != "2.330.0" {
		t.Errorf("latest ready x64 = %s, want 2.330.0", b.Version)
	}
	if _, err := s.LatestReadyBundle(ctx, "riscv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no bundle: err = %v", err)
	}
}
