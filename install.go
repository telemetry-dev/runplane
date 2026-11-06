// Package runplane holds the files at the root of the repository that the
// binary embeds.
package runplane

import _ "embed"

// InstallScript is install.sh. The primary serves it at /install.sh with
// its own values filled in.
//
//go:embed install.sh
var InstallScript []byte

// System is an operating system of a server, by the ID and the VERSION_ID
// of its /etc/os-release.
type System struct {
	ID      string // for example "ubuntu"
	Name    string // for example "Ubuntu"
	Version string // for example "24.04"
	// BestEffort: install.sh installs runplane with a warning, because
	// runplane has no tests on the system.
	BestEffort bool
}

// Systems are the systems that install.sh names for a server.
var Systems = []System{
	{ID: "ubuntu", Name: "Ubuntu", Version: "22.04"},
	{ID: "ubuntu", Name: "Ubuntu", Version: "24.04"},
	{ID: "debian", Name: "Debian", Version: "12", BestEffort: true},
	{ID: "debian", Name: "Debian", Version: "13", BestEffort: true},
}
