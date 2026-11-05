//go:build !linux

package scratch

import (
	"errors"
	"fmt"
)

var errLinuxOnly = fmt.Errorf("scratch: %w: linux only", errors.ErrUnsupported)

func (d *Disks) Create(path string, sizeGB int) error { return errLinuxOnly }
func Attach(path, mountpoint string) error            { return errLinuxOnly }
func Detach(mountpoint string) error                  { return errLinuxOnly }
func Usage(dir string) (int64, error)                 { return 0, errLinuxOnly }
func Used(mountpoint string) (int64, error)           { return 0, errLinuxOnly }
