// Package sysbox is the container mode driver. Each sandbox is one Docker
// container with the sysbox-runc runtime, on the Docker bridge network
// "runplane". With the Dev profile, the devdocker package runs the same
// driver on plain Docker.
package sysbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
	"github.com/runplane/runplane/internal/sandbox/lifecycle"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/scratch"
)

// Profile is how the sandboxes of a mode run on Docker.
type Profile struct {
	mode    sandbox.Mode
	runtime string // the OCI runtime of the containers
	// privileged lets dockerd and the overlay mounts operate under runc.
	privileged bool
	// loopScratch puts the scratch disk in a sparse ext4 file on a loop
	// device of the host, bind-mounted at /mnt/scratch. Else the scratch is
	// a Docker volume.
	loopScratch  bool
	cgroupParent string // empty is the Docker default
	// firewall puts the sandboxes behind the rules of Options.NetRules, and
	// binds each bridge port to the addresses of its container.
	firewall bool
	// watchdog aborts hung sysbox-fs connections (watchdog.go).
	watchdog bool
	// serviceHost is the address of the cache server and the registry
	// mirror for the sandboxes. Empty is the gateway of the runplane
	// network. A name points to the Docker host.
	serviceHost string
	// tcpControl serves the control channels of all sandboxes on the service
	// port + 1, because a Docker in a VM, as on macOS, cannot share Unix
	// sockets with the host.
	tcpControl bool
}

var (
	// Container is the profile of container mode.
	Container = Profile{
		mode: sandbox.ModeContainer, runtime: "sysbox-runc", loopScratch: true, cgroupParent: Slice,
		firewall: true, watchdog: true,
	}
	// Dev is the profile of dev mode: plain Docker, with each sandbox root on
	// the Docker host. The devdocker package has the rules of the mode.
	Dev = Profile{
		mode: sandbox.ModeDev, runtime: "runc", privileged: true, serviceHost: "host.docker.internal", tcpControl: true,
	}
)

// LabelSandbox marks the containers and volumes of runplane sandboxes with
// their sandbox ID. The driver changes only objects with this label.
const LabelSandbox = "dev.runplane.sandbox"

// labelData marks the containers and volumes of a driver with its data
// folder, so that two runplane processes on one Docker leave each other's
// sandboxes alone.
const labelData = "dev.runplane.data"

// Slice holds the containers of container mode. install.sh writes its unit.
const Slice = "runplane.slice"

type Options struct {
	Docker           *client.Client
	DataDir          string // the data disk; its path also marks the Docker objects of the driver
	Sandboxes        string // the sandboxes folder
	ScratchTemplates string
	ServicePort      int
	Profile          Profile
	// NetRules is the firewall of the host. The Container profile needs it.
	NetRules *netrules.Manager
	// Runner returns the folder of a ready runner bundle, and if the bundle
	// has the runner patch (guest.Patch).
	Runner func(ctx context.Context, version, arch string) (dir string, patched bool, err error)
	// InitPath returns the runplane-init binary. The driver asks at each
	// start, because a temp folder cleaner can remove the file.
	InitPath func() (string, error)
	Settings sandbox.Settings
	Logger   *slog.Logger
}

// Driver operates the sandbox containers of one server.
type Driver struct {
	*lifecycle.Core[instance]
	opts        Options
	docker      *client.Client
	log         *slog.Logger
	disks       *scratch.Disks
	serviceHost string
	tcp         *control.TCPListener // with Profile.tcpControl
	dog         *watchdog            // with Profile.watchdog
}

// instance is the Docker part of a sandbox: its container, with the name
// containerName(sandbox ID).
type instance struct {
	mu   sync.Mutex
	id   string // empty until Create makes the container
	port string // host end of the veth pair on the bridge
}

func (c *instance) get() (id, port string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id, c.port
}

var _ sandbox.Driver = (*Driver)(nil)

// New sets up the runplane network and its firewall rules, takes over the
// sandboxes that still operate, and removes the remains of the others.
func New(opts Options) (*Driver, error) {
	p := opts.Profile
	switch {
	case p.mode == "":
		return nil, errors.New("sysbox: no profile")
	case p.firewall && opts.NetRules == nil:
		return nil, fmt.Errorf("sysbox: %s mode needs the firewall rules", p.mode)
	}
	d := &Driver{
		opts:   opts,
		docker: opts.Docker,
		log:    opts.Logger,
		disks:  scratch.New(opts.ScratchTemplates),
	}
	d.Core = lifecycle.New(lifecycle.Options[instance]{
		Mode: p.mode, DataDir: opts.DataDir, Dir: opts.Sandboxes, Backend: backend{d}, Settings: opts.Settings, Logger: opts.Logger,
	})
	if err := d.setup(); err != nil {
		d.Close()
		return nil, err
	}
	d.Go(d.watchDocker)
	d.Go(d.tick)
	return d, nil
}

func (d *Driver) setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := d.opts.Profile
	if err := os.MkdirAll(d.opts.Sandboxes, 0o700); err != nil {
		return fmt.Errorf("make sandboxes folder: %w", err)
	}
	net, err := d.ensureNetwork(ctx)
	if err != nil {
		return err
	}
	d.serviceHost = cmp.Or(p.serviceHost, net.gateway.String())
	if p.firewall {
		if err := d.Configure(ctx, d.Settings()); err != nil {
			return err
		}
		if err := d.opts.NetRules.Bridge(net.bridge, net.subnet, net.gateway); err != nil {
			return fmt.Errorf("apply network rules: %w", err)
		}
	}
	if p.tcpControl {
		if d.tcp, err = control.ListenTCP(ControlAddr(d.opts.ServicePort), d.serviceHost, d.log); err != nil {
			return err
		}
	}
	if p.watchdog {
		d.dog = newWatchdog()
	}
	return d.recover(ctx)
}

// Configure takes new settings. New sandboxes get the DNS servers and the
// cache and mirror settings; the firewall policy changes at once.
func (d *Driver) Configure(ctx context.Context, s sandbox.Settings) error {
	d.SetSettings(s)
	if !d.opts.Profile.firewall {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := d.opts.NetRules.Setup(ctx, netrules.Config{
		ServicePort: d.opts.ServicePort,
		DNS:         s.IPv4DNS(),
		Allowlist:   s.EgressAllowlist,
	})
	if err != nil {
		return fmt.Errorf("apply network rules: %w", err)
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context) error {
	return Probe(ctx, ProbeOptions{Docker: d.docker, Profile: d.opts.Profile, DataDir: d.opts.DataDir, DNS: d.Settings().IPv4DNS()})
}

// Close stops the loops and the control channels. The containers keep
// operating, and a new driver takes them over.
func (d *Driver) Close() error {
	err := d.Core.Close()
	if d.tcp != nil {
		d.tcp.Close()
	}
	return err
}

// owned limits a Docker filter to the objects of this driver.
func (d *Driver) owned(f client.Filters) client.Filters {
	return f.Add("label", labelData+"="+d.opts.DataDir)
}

// byContainer returns the sandbox of a Docker container ID, or nil.
func (d *Driver) byContainer(containerID string) *box {
	for _, b := range d.Boxes() {
		if id, _ := b.Host.get(); id == containerID {
			return b
		}
	}
	return nil
}

// ControlAddr is the TCP address of the control channels of the Dev
// profile: the port after the service port.
func ControlAddr(servicePort int) string { return ":" + strconv.Itoa(servicePort+1) }

func serviceURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func mkdir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("make folder: %w", err)
	}
	// MkdirAll applies the umask.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set folder mode: %w", err)
	}
	return nil
}
