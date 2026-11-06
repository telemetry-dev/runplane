//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"net/netip"

	"github.com/runplane/runplane/internal/sandbox/netrules"
)

func (h systemHost) probe(context.Context, netrules.Lease, netip.Addr) error {
	return errors.ErrUnsupported
}
