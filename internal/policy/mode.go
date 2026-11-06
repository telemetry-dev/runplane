package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

var (
	ErrNoMode      = errors.New("no sandbox mode: set RUNPLANE_MODE to container, vm, or dev")
	ErrDevInsecure = errors.New("mode dev runs jobs in privileged containers; set RUNPLANE_DEV_INSECURE=1 to allow it on a development machine")
	ErrNoKVM       = errors.New("VM mode needs KVM, and the primary server has no /dev/kvm")
)

// Mode is the sandbox mode of the primary: the mode setting, or before the
// first start the mode of the configuration (RUNPLANE_MODE). The first start
// keeps that mode in the settings, so that the dashboard and set-mode can
// change it later.
func Mode(st store.Settings, configured sandbox.Mode) sandbox.Mode {
	return cmp.Or(st.Mode, configured)
}

// CheckMode tells why runplane cannot operate sandboxes in mode m.
// devInsecure is RUNPLANE_DEV_INSECURE.
func CheckMode(m sandbox.Mode, devInsecure bool) error {
	switch {
	case m == "":
		return ErrNoMode
	case m == sandbox.ModeDev && !devInsecure:
		return ErrDevInsecure
	}
	return m.Check()
}

// CheckModeChange tells why the primary cannot change to mode m. VM mode
// also needs KVM on the primary.
func CheckModeChange(m sandbox.Mode, devInsecure, kvm bool) error {
	if m == sandbox.ModeVM && !kvm {
		return ErrNoKVM
	}
	return CheckMode(m, devInsecure)
}

// PublicRepos tells why an admin cannot turn allow_public_repos on with the
// mode setting m and the servers, or returns "" when the admin can. It names
// each server that is not in VM mode: while the setting is on, these servers
// wait (refer to RunnerRefusal). A server reports the mode of its running
// driver, which can differ from the mode setting until a restart. A server
// without facts does not count yet.
func PublicRepos(m sandbox.Mode, servers []store.Server) string {
	if m != sandbox.ModeVM {
		return fmt.Sprintf("Jobs of public repositories need VM mode, and the mode setting is %s.", cmp.Or(string(m), "not set"))
	}
	var others []string
	for _, v := range servers {
		if v.Mode != "" && v.Mode != sandbox.ModeVM {
			others = append(others, fmt.Sprintf("%s runs %s mode", v.Name, v.Mode))
		}
	}
	if len(others) == 0 {
		return ""
	}
	return "Jobs of public repositories need VM mode on each server, and " + strings.Join(others, ", ") + "."
}

// BandwidthApplies tells if the bandwidth setting limits sandboxes: only the
// VMs have the limit, so the mode setting or a server must be in VM mode.
func BandwidthApplies(m sandbox.Mode, servers []store.Server) bool {
	return m == sandbox.ModeVM || slices.ContainsFunc(servers, func(v store.Server) bool { return v.Mode == sandbox.ModeVM })
}
