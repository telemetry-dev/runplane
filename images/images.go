// Package images embeds the Dockerfile folders of the builtin base images.
// Each top folder of FS is one image name, for example "ubuntu-24.04".
package images

import "embed"

//go:embed all:ubuntu-24.04 all:ubuntu-26.04
var FS embed.FS

// Default is the default image of a new install.
const Default = "ubuntu-24.04"
