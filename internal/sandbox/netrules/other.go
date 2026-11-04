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

func New(*slog.Logger) *Manager { return &Manager{} }

func (m *Manager) Setup(context.Context, Config) error           { return errLinuxOnly }
func (m *Manager) AddTap(string, netip.Addr, netip.Addr) error   { return errLinuxOnly }
func (m *Manager) RemoveTap(string) error                        { return errLinuxOnly }
func (m *Manager) Bridge(string, netip.Prefix, netip.Addr) error { return errLinuxOnly }
func (m *Manager) AddPort(string, net.HardwareAddr, netip.Addr) error {
	return errLinuxOnly
}
func (m *Manager) RemovePort(string) error           { return errLinuxOnly }
func HostPeer(string, string) (string, error)        { return "", errLinuxOnly }
func (m *Manager) Close() error                      { return nil }
func (m *Manager) Teardown() error                   { return errLinuxOnly }
func CreateTap(string, int, int, netip.Prefix) error { return errLinuxOnly }
func DeleteTap(string) error                         { return errLinuxOnly }
func (a *Allocator) Recover() error                  { return errLinuxOnly }
