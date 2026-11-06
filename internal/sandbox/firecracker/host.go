package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// hostOps are the changes that the driver makes to the host network and
// to systemd.
type hostOps interface {
	setupNet(ctx context.Context, cfg netrules.Config) error
	acquireTap() (netrules.Lease, error)
	tapLeases() int
	// openTap makes the TAP of a lease, which only uid and gid can open,
	// and lets it through the firewall.
	openTap(l netrules.Lease, uid, gid int) error
	// keepTap lets the TAP of a VM from before a restart through again.
	keepTap(l netrules.Lease) error
	closeTap(l netrules.Lease) error
	killScope(unit string) error
	// stopJobs returns the VM scopes that systemd is about to stop, as at
	// a host shutdown.
	stopJobs() (map[string]bool, error)
	// probe fetches sandbox.ProbeURL with the network of a VM on lease l.
	probe(ctx context.Context, l netrules.Lease, dns netip.Addr) error
}

type systemHost struct{ net *netrules.Manager }

func (h systemHost) setupNet(ctx context.Context, cfg netrules.Config) error {
	return h.net.Setup(ctx, cfg)
}

func (h systemHost) acquireTap() (netrules.Lease, error) { return h.net.AcquireTap() }
func (h systemHost) tapLeases() int                      { return h.net.TapLeases() }

func (h systemHost) openTap(l netrules.Lease, uid, gid int) error { return h.net.OpenTap(l, uid, gid) }
func (h systemHost) keepTap(l netrules.Lease) error               { return h.net.KeepTap(l) }
func (h systemHost) closeTap(l netrules.Lease) error              { return h.net.CloseTap(l) }

func (h systemHost) killScope(unit string) error {
	out, err := exec.Command("systemctl", "kill", "--signal=SIGKILL", unit+".scope").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill %s: %w: %s", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

func (h systemHost) stopJobs() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "list-jobs", "--no-legend", unitName("*")+".scope").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-jobs: %w", err)
	}
	return parseStopJobs(out), nil
}

// parseStopJobs reads the output of systemctl list-jobs: the lines
// "<job> <unit> <type> <state>".
func parseStopJobs(out []byte) map[string]bool {
	units := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) == 4 && f[2] == "stop" {
			units[f[1]] = true
		}
	}
	return units
}
