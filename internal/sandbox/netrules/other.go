//go:build !linux

package netrules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
)

var errLinuxOnly = fmt.Errorf("netrules: %w: linux only", errors.ErrUnsupported)

// Manager is a stub: the firewall rules exist on Linux only.
type Manager struct{}

func New(*slog.Logger, netip.Prefix) *Manager { return &Manager{} }

func (m *Manager) Setup(context.Context, Config) error           { return errLinuxOnly }
func (m *Manager) AcquireTap() (Lease, error)                    { return Lease{}, errLinuxOnly }
func (m *Manager) TapLeases() int                                { return 0 }
func (m *Manager) OpenTap(Lease, int, int) error                 { return errLinuxOnly }
func (m *Manager) KeepTap(Lease) error                           { return errLinuxOnly }
func (m *Manager) CloseTap(Lease) error                          { return errLinuxOnly }
func (m *Manager) Bridge(string, netip.Prefix, netip.Addr) error { return errLinuxOnly }
func (m *Manager) AddPort(string, net.HardwareAddr, netip.Addr) error {
	return errLinuxOnly
}
func (m *Manager) RemovePort(string) error    { return errLinuxOnly }
func HostPeer(string, string) (string, error) { return "", errLinuxOnly }
func (m *Manager) Close() error               { return nil }
