//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetImageEnv(t *testing.T) {
	// t.Setenv puts the old values back after the test.
	t.Setenv("RUNNER_TOOL_CACHE", "")
	t.Setenv("GOFLAGS", "")
	file := filepath.Join(t.TempDir(), "environ")
	err := os.WriteFile(file, []byte("RUNNER_TOOL_CACHE=/opt/hostedtoolcache\x00GOFLAGS=-tags=vm\x00"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := setImageEnv(file); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"RUNNER_TOOL_CACHE": "/opt/hostedtoolcache", "GOFLAGS": "-tags=vm"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
