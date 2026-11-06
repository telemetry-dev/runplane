// Package images holds the builtin base images of the sandboxes. One
// Dockerfile builds each of them with its BuildArgs. The images workflow
// (.github/workflows/images.yml) publishes them at Ref, and images/README.md
// lists them. images_test.go compares these files with Names.
package images

import (
	"embed"
	"slices"
	"strings"
)

// FS holds the Dockerfile, which is the build context of each builtin image.
//
//go:embed Dockerfile
var FS embed.FS

// Names are the builtin images. Each name is "ubuntu-<release>".
var Names = []string{"ubuntu-24.04", "ubuntu-26.04"}

// Default is the default image of a new install.
const Default = "ubuntu-24.04"

const (
	// Prefix starts the ref of the image row of a builtin image:
	// "builtin:<name>".
	Prefix = "builtin:"
	// Source is the source of the image row of a builtin image.
	Source = "builtin"
)

// Name returns the builtin image that the ref of an image row names.
func Name(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, Prefix)
	return name, ok && slices.Contains(Names, name)
}

// Ref returns the registry reference of a builtin image that the images
// workflow of the repository repo publishes.
func Ref(repo, name string) string {
	return "ghcr.io/" + strings.ToLower(repo) + "/runner-" + name + ":latest"
}

// BuildArgs returns the build args of the Dockerfile for a builtin image.
// IMAGE_OS is the ImageOS of the GitHub-hosted runners, for example
// "ubuntu24".
func BuildArgs(name string) map[string]string {
	release := strings.TrimPrefix(name, "ubuntu-")
	major, _, _ := strings.Cut(release, ".")
	return map[string]string{"UBUNTU": release, "IMAGE_OS": "ubuntu" + major}
}
