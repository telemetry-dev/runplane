// Package devdocker is the development driver: the container mode driver
// with the sysbox.Dev profile, on plain Docker (runc with --privileged)
// instead of Sysbox. A privileged container is root on the Docker host, so
// this driver is never for production. It operates only with
// RUNPLANE_DEV_INSECURE=1.
//
// Docker sends the requests of all dev sandboxes to runplane from one
// address, so runplane cannot tell the sandboxes apart: they get no Actions
// cache, and the service port takes the loopback address (Allow).
package devdocker

import (
	"context"
	"errors"
	"net/netip"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
)

type Driver struct{ *sysbox.Driver }

// New makes the driver with opts and the sysbox.Dev profile. devInsecure is
// the RUNPLANE_DEV_INSECURE setting.
func New(devInsecure bool, opts sysbox.Options) (*Driver, error) {
	if !devInsecure {
		return nil, errors.New("devdocker: each dev sandbox is root on the Docker host; set RUNPLANE_DEV_INSECURE=1 to use this driver")
	}
	opts.Logger.Warn("dev mode: sandboxes get no Actions cache, because the cache server cannot tell them apart by address")
	opts.Profile = sysbox.Dev
	opts.Settings = noCache(opts.Settings)
	drv, err := sysbox.New(opts)
	if err != nil {
		return nil, err
	}
	return &Driver{drv}, nil
}

func (d *Driver) Configure(ctx context.Context, s sandbox.Settings) error {
	return d.Driver.Configure(ctx, noCache(s))
}

// Requirements are what dev mode needs on the server, for the doctor.
// envFile is the environment file, which sets the mode.
func Requirements(servicePort int, envFile string) doctor.Requirements {
	return doctor.Requirements{
		Mode: doctor.ModeCheck.Warn("Dev mode runs jobs in privileged Docker containers",
			"Set RUNPLANE_MODE to container or vm in "+envFile+" on a production server."),
		Docker: true,
		Listen: []string{sysbox.ControlAddr(servicePort)},
	}
}

// Allow adds the loopback address to allow, which tells if an address at the
// service port is a sandbox.
func Allow(allow func(netip.Addr) bool) func(netip.Addr) bool {
	return func(ip netip.Addr) bool { return ip.IsLoopback() || allow(ip) }
}

func noCache(s sandbox.Settings) sandbox.Settings {
	s.ActionsCache = false
	return s
}
