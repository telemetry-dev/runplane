package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// apiClient calls the Firecracker API on its Unix socket.
type apiClient struct{ http *http.Client }

func newAPIClient(socket string) *apiClient {
	var dialer net.Dialer
	return &apiClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (a *apiClient) call(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var fault struct {
		Message string `json:"fault_message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&fault)
	return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, fault.Message)
}

// boot sets up the VM through the API and starts it. The paths are inside
// the chroot.
func (a *apiClient) boot(ctx context.Context, spec sandbox.Spec, l netrules.Lease, bandwidthMbps int) error {
	type step struct {
		path string
		body any
	}
	steps := []step{{"/boot-source", bootSource{KernelImagePath: "/" + kernelFile, BootArgs: bootArgs(l.Guest, l.Host)}}}
	drives := [...]drive{
		guest.DriveRoot:    {ID: "rootfs", Path: "/" + rootFile, Root: true, ReadOnly: true},
		guest.DriveRunner:  {ID: "runner", Path: "/" + runnerFile, ReadOnly: true},
		guest.DriveScratch: {ID: "scratch", Path: "/" + scratchFile},
	}
	// The guest names the drives in the order of these calls.
	for _, d := range drives {
		steps = append(steps, step{"/drives/" + d.ID, d})
	}
	steps = append(steps, []step{
		{"/machine-config", machineConfig{VCPU: spec.VCPU, MemoryMiB: spec.MemoryMB}},
		{"/network-interfaces/eth0", netInterface{
			ID:      "eth0",
			HostDev: l.Tap,
			MAC:     guestMAC(l.Guest),
			RX:      bandwidthLimit(bandwidthMbps),
			TX:      bandwidthLimit(bandwidthMbps),
		}},
		// Unix sockets back the vsock of Firecracker, so all VMs can
		// have CID 3.
		{"/vsock", vsock{CID: 3, Path: "/" + vsockFile}},
		{"/entropy", struct{}{}},
		{"/actions", action{Type: "InstanceStart"}},
	}...)
	for _, s := range steps {
		if err := a.call(ctx, http.MethodPut, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

// setBandwidth changes the network limit of a running VM. 0 removes it.
func (a *apiClient) setBandwidth(ctx context.Context, mbps int) error {
	limit := bandwidthLimit(mbps)
	if limit == nil {
		// A bucket of size 0 turns the limiter off.
		limit = &rateLimiter{Bandwidth: tokenBucket{}}
	}
	return a.call(ctx, http.MethodPatch, "/network-interfaces/eth0", netInterface{ID: "eth0", RX: limit, TX: limit})
}

func (a *apiClient) ctrlAltDel(ctx context.Context) error {
	return a.call(ctx, http.MethodPut, "/actions", action{Type: "SendCtrlAltDel"})
}

// bootArgs is the guest kernel command line. Firecracker adds
// "root=/dev/vda ro" for the read-only root drive and, from v1.13, "pci=off".
// quiet keeps the boot messages off the serial console, where each byte
// stops the vCPU: the guest then reaches init in half the time. Warnings and
// panics still print. ip= sets the guest address without DHCP.
func bootArgs(addr, host netip.Addr) string {
	return "console=ttyS0 quiet " + guest.KernelArgs +
		" ip=" + addr.String() + "::" + host.String() + ":255.255.255.252::eth0:off random.trust_cpu=on"
}

// guestMAC is a locally administered address that holds the guest IPv4
// address.
func guestMAC(guest netip.Addr) string {
	a := guest.As4()
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", a[0], a[1], a[2], a[3])
}

// bandwidthLimit returns a limiter of mbps in each direction, or nil for 0.
func bandwidthLimit(mbps int) *rateLimiter {
	if mbps <= 0 {
		return nil
	}
	// Bytes per second; the bucket refills each second.
	return &rateLimiter{Bandwidth: tokenBucket{Size: int64(mbps) * 125_000, RefillTime: 1000}}
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
}

type machineConfig struct {
	VCPU      int `json:"vcpu_count"`
	MemoryMiB int `json:"mem_size_mib"`
}

type netInterface struct {
	ID      string       `json:"iface_id"`
	HostDev string       `json:"host_dev_name,omitempty"`
	MAC     string       `json:"guest_mac,omitempty"`
	RX      *rateLimiter `json:"rx_rate_limiter,omitempty"`
	TX      *rateLimiter `json:"tx_rate_limiter,omitempty"`
}

type rateLimiter struct {
	Bandwidth tokenBucket `json:"bandwidth"`
}

type tokenBucket struct {
	Size       int64 `json:"size"`
	RefillTime int64 `json:"refill_time"` // milliseconds
}

type vsock struct {
	CID  int    `json:"guest_cid"`
	Path string `json:"uds_path"`
}

type action struct {
	Type string `json:"action_type"`
}
