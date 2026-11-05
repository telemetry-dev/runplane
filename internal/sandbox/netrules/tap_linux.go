//go:build linux

package netrules

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// createTap makes a persistent TAP device that only a process with the uid
// and the gid can open, gives the host side the address hostCIDR, and sets
// it up. It replaces a device with the same name.
func createTap(name string, uid, gid int, hostCIDR netip.Prefix) error {
	if err := deleteTap(name); err != nil {
		return err
	}
	if err := makeTap(name, uid, gid); err != nil {
		return fmt.Errorf("create TAP %s: %w", name, err)
	}
	if err := configureTap(name, hostCIDR); err != nil {
		return errors.Join(fmt.Errorf("configure TAP %s: %w", name, err), deleteTap(name))
	}
	return nil
}

// deleteTap removes a TAP device. A missing device is not an error.
func deleteTap(name string) error {
	if !isTap(name) || !validIfname(name) {
		return fmt.Errorf("netrules: %q is not a TAP name", name)
	}
	l, err := netlink.LinkByName(name)
	if errors.As(err, new(netlink.LinkNotFoundError)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find TAP %s: %w", name, err)
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete TAP %s: %w", name, err)
	}
	return nil
}

// makeTap does what "ip tuntap add mode tap user <uid> group <gid>" does.
// The kernel then lets a process open the device only with the uid and the
// gid. netlink.Tuntap sets the group to root, which no VM has.
func makeTap(name string, uid, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("TUNSETIFF: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return fmt.Errorf("TUNSETOWNER: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, gid); err != nil {
		return fmt.Errorf("TUNSETGROUP: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("TUNSETPERSIST: %w", err)
	}
	return nil
}

func configureTap(name string, hostCIDR netip.Prefix) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	// Sandboxes have IPv4 only. Without IPv6 the host sends no router
	// solicitations or other link-local traffic to the guest.
	err = os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("1\n"), 0o644)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ipnet := &net.IPNet{IP: hostCIDR.Addr().AsSlice(), Mask: net.CIDRMask(hostCIDR.Bits(), 32)}
	if err := netlink.AddrReplace(l, &netlink.Addr{IPNet: ipnet}); err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}
