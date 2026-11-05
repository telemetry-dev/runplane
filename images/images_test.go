package images

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The images workflow builds and publishes the builtin images, and the
// README lists them. A new image or a new build arg must change
// images.go, .github/workflows/images.yml, and images/README.md together.
func TestWorkflowBuildsTheBuiltinImages(t *testing.T) {
	yml, err := os.ReadFile("../.github/workflows/images.yml")
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`(?m)^\s+- \{name: (\S+), ubuntu: '([^']+)', image_os: (\S+)\}$`)
	var built []string
	for _, m := range entry.FindAllStringSubmatch(string(yml), -1) {
		built = append(built, m[1])
		want := BuildArgs(m[1])
		if got := map[string]string{"UBUNTU": m[2], "IMAGE_OS": m[3]}; !maps.Equal(got, want) {
			t.Errorf("images.yml builds %s with %v, images.go gives %v", m[1], got, want)
		}
	}
	if !slices.Equal(built, Names) {
		t.Errorf("the build matrix of images.yml has %v, want %v", built, Names)
	}
	merged := regexp.MustCompile(`(?m)^\s+image: \[(.*)\]$`).FindStringSubmatch(string(yml))
	if merged == nil || !slices.Equal(strings.Split(merged[1], ", "), Names) {
		t.Errorf("the merge matrix of images.yml has %q, want %v", merged, Names)
	}

	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	// A build arg is in scope only after its ARG in the stage.
	_, stage, _ := strings.Cut(string(dockerfile), "\nFROM ")
	for arg := range BuildArgs(Default) {
		if !strings.Contains(stage, "\nARG "+arg+"\n") {
			t.Errorf("the Dockerfile has no ARG %s after FROM", arg)
		}
	}
}

func TestReadmeListsTheBuiltinImages(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	def := ""
	for _, m := range regexp.MustCompile("(?m)^\\| `(ghcr\\.io/[^`]+)` \\| (.*) \\|$").FindAllStringSubmatch(string(readme), -1) {
		refs = append(refs, m[1])
		if strings.Contains(m[2], "default") {
			def = m[1]
		}
	}
	var want []string
	for _, name := range Names {
		want = append(want, Ref("runplane/runplane", name))
	}
	if !slices.Equal(refs, want) {
		t.Errorf("the README table has %v, want %v", refs, want)
	}
	if want := Ref("runplane/runplane", Default); def != want {
		t.Errorf("the README marks %q as the default, want %q", def, want)
	}
}
