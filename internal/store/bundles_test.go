package store

import (
	"strings"
	"testing"
)

// The version order is numeric: 2.330.0 is newer than 2.99.0.
func TestListRunnerBundlesNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, b := range []RunnerBundle{
		{Version: "2.99.0", Arch: "x64", Status: BundleReady},
		{Version: "2.330.0", Arch: "x64", Status: BundleReady},
		{Version: "2.331.0", Arch: "x64", Status: BundleFailed},
		{Version: "2.330.0", Arch: "arm64", Status: BundleReady},
	} {
		must(t, s.SetRunnerBundleBuild(ctx, b))
	}
	list, err := s.ListRunnerBundles(ctx)
	must(t, err)
	var got []string
	for _, b := range list {
		got = append(got, b.Version+"-"+b.Arch)
	}
	want := "2.331.0-x64 2.330.0-arm64 2.330.0-x64 2.99.0-x64"
	if s := strings.Join(got, " "); s != want {
		t.Errorf("bundles = %s, want %s", s, want)
	}
}
