package firecracker

import (
	"context"
	"errors"
	"time"
)

// probeURL is what the internet probe fetches, as in container mode.
const probeURL = "https://api.github.com"

// Probe does the sandbox internet check of the doctor. It does not boot a
// VM: it puts a network namespace behind a free TAP address, with the
// firewall rules of a VM, and fetches probeURL from there through the DNS
// server of the settings. Thus it checks the host part of the VM network:
// forwarding, NAT, the runplane rules, and other firewalls of the host.
func (d *Driver) Probe(ctx context.Context) error {
	d.mu.Lock()
	s := d.settings
	d.mu.Unlock()
	dns := ipv4DNS(s)
	if len(dns) == 0 {
		return errors.New("the dns setting has no IPv4 server, and VMs have IPv4 only")
	}
	lease, err := d.taps.Acquire()
	if err != nil {
		return err
	}
	defer d.taps.Release(lease.Index)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return d.sys.probe(ctx, lease, dns[0], probeURL)
}
