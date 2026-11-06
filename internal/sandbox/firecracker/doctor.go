package firecracker

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// The programs and the user of the VMs on the host, which install.sh
// installs.
const (
	firecrackerBin = "/usr/local/bin/firecracker"
	jailerBin      = "/usr/local/bin/jailer"
	// jailerUser has the ranges of the uids and gids of the VMs in subuidFile
	// and subgidFile.
	jailerUser = "runplane-vm"
	subuidFile = "/etc/subuid"
	subgidFile = "/etc/subgid"
)

// Requirements are what VM mode needs on the server, for the doctor. kernel
// is the guest kernel, and jailDir the folder of the chroots.
func Requirements(kernel, jailDir string) doctor.Requirements {
	return doctor.Requirements{
		Mode: doctor.ModeCheck.OK("VM mode (Firecracker)"),
		Software: func(ctx context.Context, h *doctor.Host) {
			h.FirecrackerVersion = doctor.ToolVersion(ctx, firecrackerBin)
		},
		Checks: func(ctx context.Context, h *doctor.Host) []doctor.Check {
			f := vmHost{Host: h, guestKernel: kernel, jailDir: jailDir}
			f.jailerVersion = doctor.ToolVersion(ctx, jailerBin)
			f.guestKernelErr = checkKernelFile(kernel)
			f.ids, f.idsErr = readIDRange(jailerUser, subuidFile, subgidFile)
			f.tunErr = checkTun()
			f.mount, f.mountErr = readMountOptions(jailDir)
			f.firewall = doctor.ReadFirewall(ctx, h.Root)
			return f.checks()
		},
	}
}

type vmHost struct {
	*doctor.Host
	jailerVersion  string
	guestKernel    string
	guestKernelErr error
	ids            idRange
	idsErr         error
	tunErr         error
	jailDir        string
	mount          []string // the options of the mount of jailDir
	mountErr       error
	firewall       doctor.Firewall
}

func (h vmHost) checks() []doctor.Check {
	return []doctor.Check{
		h.osCheck(), h.KernelCheck(h.kernelRules), h.CgroupCheck(), h.kvmCheck(), h.NFTablesCheck(), h.ForwardCheck(),
		h.firecrackerCheck(), h.guestKernelCheck(), h.idsCheck(), h.tunCheck(), h.mountCheck(), h.firewallCheck(),
		h.SystemdCheck("which keeps VMs in scopes"),
	}
}

func (h vmHost) osCheck() doctor.Check {
	return doctor.Check{ID: "os", Name: "Operating system"}.OK(cmp.Or(h.OS, "Unknown"))
}

func (h vmHost) kernelRules(c doctor.Check, v doctor.Version) doctor.Check {
	if v.Less(doctor.Version{5, 10}) {
		return c.Fail(h.Kernel+": Firecracker needs kernel 5.10 or newer", "Upgrade the kernel to 6.1 or newer.")
	}
	switch [2]int{v[0], v[1]} {
	case [2]int{5, 10}, [2]int{6, 1}, [2]int{6, 18}:
		return c.OK(h.Kernel)
	}
	return c.OK(h.Kernel + " (Firecracker tests host kernels 5.10, 6.1, and 6.18)")
}

func (h vmHost) kvmCheck() doctor.Check {
	c := doctor.Check{ID: "kvm", Name: "KVM"}
	switch {
	case h.KVM && h.Virtual && h.Provider == host.ProviderOVH:
		return c.Warn("/dev/kvm works, but a live migration on OVHcloud Public Cloud can crash nested VMs", "Use an OVHcloud bare metal server, or container mode.")
	case h.KVM && h.Virtual:
		return c.OK("/dev/kvm opens read-write (nested virtualization: VMs are slower than on bare metal)")
	case h.KVM:
		return c.OK("/dev/kvm opens read-write")
	case errors.Is(h.KVMErr, fs.ErrPermission) && !h.Root:
		return c.AsRoot("No permission to open /dev/kvm")
	case errors.Is(h.KVMErr, fs.ErrNotExist) && h.CPUVirt:
		return c.Fail("/dev/kvm is missing, but the CPU supports virtualization", "Load the KVM module of the CPU: kvm_intel or kvm_amd.").
			WithCommand("sudo modprobe kvm_intel || sudo modprobe kvm_amd")
	case errors.Is(h.KVMErr, fs.ErrNotExist):
		return c.Fail("/dev/kvm is missing", "Use container mode, or a server with KVM.")
	}
	return c.Fail(fmt.Sprintf("Cannot open /dev/kvm: %v", h.KVMErr), "Use container mode, or a server with KVM.")
}

func (h vmHost) firecrackerCheck() doctor.Check {
	c := doctor.Check{ID: "firecracker", Name: "Firecracker"}
	switch {
	case h.FirecrackerVersion == "":
		return c.Fail("Cannot run "+firecrackerBin, "Run the install script again.")
	case h.jailerVersion == "":
		return c.Fail("Cannot run "+jailerBin, "Run the install script again.")
	case h.FirecrackerVersion != h.jailerVersion:
		return c.Fail(fmt.Sprintf("firecracker %s and jailer %s differ", h.FirecrackerVersion, h.jailerVersion), "Install firecracker and jailer from the same release.")
	}
	return c.OK(fmt.Sprintf("Firecracker %s and jailer %s", h.FirecrackerVersion, h.jailerVersion))
}

func (h vmHost) guestKernelCheck() doctor.Check {
	c := doctor.Check{ID: "guest_kernel", Name: "Guest kernel"}
	if h.guestKernelErr != nil {
		return c.Fail(fmt.Sprintf("Cannot use the guest kernel: %v", h.guestKernelErr), "Run the install script again to download the guest kernel.")
	}
	return c.OK(h.guestKernel)
}

func checkKernelFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fmt.Errorf("%s is not a kernel file", path)
	}
	return nil
}

func (h vmHost) idsCheck() doctor.Check {
	c := doctor.Check{ID: "vm_ids", Name: "VM user IDs"}
	if h.idsErr != nil {
		return c.Fail(fmt.Sprintf("Cannot read the IDs of the VMs: %v", h.idsErr), "Run the install script again: it adds the user "+jailerUser+" and its ID ranges.")
	}
	return c.OK(fmt.Sprintf("%s has the uids from %d and the gids from %d for %d VMs", jailerUser, h.ids.uid, h.ids.gid, h.ids.count))
}

func (h vmHost) tunCheck() doctor.Check {
	c := doctor.Check{ID: "tun", Name: "TUN device"}
	if h.tunErr != nil {
		return c.Fail(fmt.Sprintf("Cannot make the TAP devices of the VMs: %v", h.tunErr), "Load the tun module.").WithCommand("sudo modprobe tun")
	}
	return c.OK(netrules.TunDevice + " is present")
}

func checkTun() error {
	fi, err := os.Stat(netrules.TunDevice)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a character device", netrules.TunDevice)
	}
	return nil
}

// mountCheck looks at the file system of the chroots: the jailer makes the
// device nodes of each VM there, and runs a copy of Firecracker.
func (h vmHost) mountCheck() doctor.Check {
	c := doctor.Check{ID: "jail_mount", Name: "Data folder mount"}
	const fix = "Put the data folder on a file system that is not mounted with nodev or noexec."
	if h.mountErr != nil {
		return c.Warn(fmt.Sprintf("Cannot read the mount options of %s: %v", h.jailDir, h.mountErr), fix).WithCommand("findmnt --target " + h.jailDir)
	}
	var bad []string
	for _, o := range h.mount {
		if o == "nodev" || o == "noexec" {
			bad = append(bad, o)
		}
	}
	if len(bad) > 0 {
		return c.Fail(fmt.Sprintf("%s is on a file system mounted with %s, so the jailer cannot start VMs", h.jailDir, strings.Join(bad, " and ")), fix).
			WithCommand("findmnt --target " + h.jailDir)
	}
	return c.OK(h.jailDir + " allows device files and programs")
}

// readMountOptions returns the options of the mount that holds path, or its
// nearest parent that exists.
func readMountOptions(path string) ([]string, error) {
	for {
		if _, err := os.Stat(path); err == nil || path == filepath.Dir(path) {
			break
		}
		path = filepath.Dir(path)
	}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return mountOptions(f, path)
}

// mountOptions reads a mountinfo table. Of the mounts that hold path, the
// one with the longest mount point wins, and of two at the same point, the
// later one.
func mountOptions(r io.Reader, path string) ([]string, error) {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var best string
	var opts []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 6 {
			continue
		}
		point := unescape.Replace(f[4])
		holds := point == "/" || path == point || strings.HasPrefix(path, point+"/")
		if holds && len(point) >= len(best) {
			best, opts = point, strings.Split(f[5], ",")
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if opts == nil {
		return nil, fmt.Errorf("no mount holds %s", path)
	}
	return opts, nil
}

// runplane puts accept rules for its VMs at the top of INPUT and FORWARD of
// the iptables filter table, so a drop policy there, as from ufw, is fine.
func (h vmHost) firewallCheck() doctor.Check {
	c := doctor.Check{ID: "firewall", Name: "Firewall rules"}
	fw := h.firewall
	if blocked, ok := fw.Blocked(c, netrules.TapPrefix+"*"); ok {
		return blocked
	}
	switch {
	case errors.Is(fw.Err, exec.ErrNotFound):
		return c.OK("No iptables rules")
	case errors.Is(fw.Err, doctor.ErrNeedRoot):
		return c.AsRoot("Cannot read the firewall without root")
	case fw.Err != nil:
		return c.Warn(fmt.Sprintf("Cannot read the iptables rules: %v", fw.Err), "Check the policy of the FORWARD chain.").WithCommand("sudo iptables -S FORWARD")
	}
	if open, ok := fw.OpenForward(c, h.Host); ok {
		return open
	}
	switch {
	case fw.ForwardPolicy != "DROP":
		return c.OK("FORWARD policy " + cmp.Or(fw.ForwardPolicy, "ACCEPT"))
	case fw.DockerUser:
		return c.OK("Docker sets the FORWARD policy to DROP; runplane adds accept rules for its sandboxes")
	}
	return c.OK("The FORWARD policy is DROP; runplane adds accept rules for its sandboxes at the top of INPUT and FORWARD")
}
