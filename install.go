// Package runplane holds the files at the root of the repository that the
// binary embeds.
package runplane

import _ "embed"

// InstallScript is install.sh. The primary serves it at /install.sh with
// its own values filled in.
//
//go:embed install.sh
var InstallScript []byte
