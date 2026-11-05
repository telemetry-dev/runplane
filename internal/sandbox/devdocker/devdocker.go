// Package devdocker is the development driver: the container mode driver
// on plain Docker (runc with --privileged) instead of Sysbox. A privileged
// container is root on the Docker host, so this driver is never for
// production. It operates only with RUNPLANE_DEV_INSECURE=1.
package devdocker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
)

// ServiceHost is the name under which a dev sandbox reaches runplane on the
// Docker host, also through the VM of Docker Desktop or OrbStack.
const ServiceHost = "host.docker.internal"

type Options struct {
	Docker *client.Client
	// Config gives the data folders and the service port. The control
	// channel listens on the service port + 1. DevInsecure must be set.
	Config   config.Config
	Bundle   func(ctx context.Context, version, arch string) (runner.Bundle, error)
	InitPath func() (string, error)
	Settings sandbox.Settings
	Logger   *slog.Logger
}

func New(o Options) (*sysbox.Driver, error) {
	if !o.Config.DevInsecure {
		return nil, errors.New("devdocker: each dev sandbox is root on the Docker host; set RUNPLANE_DEV_INSECURE=1 to use this driver")
	}
	return sysbox.New(sysbox.Options{
		Docker:        o.Docker,
		Config:        o.Config,
		Mode:          sandbox.ModeDev,
		Runtime:       "runc",
		Privileged:    true,
		Scratch:       sysbox.ScratchVolume,
		Bundle:        o.Bundle,
		InitPath:      o.InitPath,
		ServiceHost:   ServiceHost,
		ControlListen: ":" + strconv.Itoa(o.Config.ServicePort+1),
		Settings:      o.Settings,
		Logger:        o.Logger,
	})
}
