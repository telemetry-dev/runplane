package main

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/devdocker"
	"github.com/runplane/runplane/internal/sandbox/firecracker"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/version"
)

// The systemd services of the primary and of an agent, which install.sh
// writes.
const (
	primaryUnit = "runplane"
	agentUnit   = "runplane-agent"
)

// doctorOptions returns the doctor options of this server in mode: of an
// agent, or of the primary, whose domain adds the ports of HTTPS. The
// caller adds the internet check, if it can do it, and the
// min_free_disk_percent setting.
func doctorOptions(cfg config.Config, mode sandbox.Mode, agent bool, domain string) doctor.Options {
	o := doctor.Options{
		DataDir: cfg.DataDir, EnvFile: cfg.EnvFilePath(), Unit: primaryUnit, Listen: primaryAddrs(cfg, domain),
		Requirements: requirements(cfg, mode, agent), Docker: dockerFacts, BaseImage: baseImage,
	}
	if agent {
		o.Unit, o.Listen = agentUnit, []string{servicePortAddr(cfg)}
	}
	return o
}

func requirements(cfg config.Config, mode sandbox.Mode, agent bool) doctor.Requirements {
	switch mode {
	case sandbox.ModeContainer:
		return sysbox.Requirements()
	case sandbox.ModeVM:
		return firecracker.Requirements(cfg.KernelPath(), cfg.JailDir())
	case sandbox.ModeDev:
		return devdocker.Requirements(cfg.ServicePort, modeFix(cfg, agent, "container or vm"))
	}
	// config.Parse and the settings accept only the modes above, so the
	// mode is empty.
	return doctor.Requirements{
		Mode: doctor.ModeCheck.Fail(policy.ErrNoMode.Error(), modeFix(cfg, agent, "container, vm, or dev")),
	}
}

// modeFix tells how to give this server one of modes. After the first start
// of a primary, which makes its database, the mode setting wins over
// RUNPLANE_MODE (policy.Mode).
func modeFix(cfg config.Config, agent bool, modes string) string {
	if !agent && exists(cfg.DBPath()) {
		return "Run sudo runplane set-mode " + modes + "."
	}
	return "Set RUNPLANE_MODE to " + modes + " in " + cfg.EnvFilePath() + "."
}

// primaryAddrs lists the TCP addresses of the primary. A domain adds the
// ports of HTTPS, which another web server often has.
func primaryAddrs(cfg config.Config, domain string) []string {
	addrs := []string{cfg.Listen, cfg.AgentListen, servicePortAddr(cfg), cfg.SSHListen}
	if domain != "" {
		addrs = append(addrs, tlsutil.HTTPSAddr, tlsutil.HTTPAddr)
	}
	return addrs
}

// dockerFacts reads the Docker Engine at DOCKER_HOST or the default socket.
func dockerFacts(ctx context.Context) (doctor.Docker, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := client.New(client.FromEnv)
	if err != nil {
		return doctor.Docker{}, fmt.Errorf("make docker client: %w", err)
	}
	defer c.Close()
	res, err := c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return doctor.Docker{}, fmt.Errorf("read docker info: %w", err)
	}
	d := doctor.Docker{Version: res.Info.ServerVersion, RootDir: res.Info.DockerRootDir, Runtimes: map[string]string{}}
	if fw := res.Info.FirewallBackend; fw != nil {
		d.Firewall = fw.Driver
	}
	for name, rt := range res.Info.Runtimes {
		d.Runtimes[name] = rt.Path
	}
	return d, nil
}

func baseImage(ctx context.Context) (string, error) {
	ref := images.Ref(version.Repo, images.Default)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ref, image.CanPull(ctx, ref)
}
