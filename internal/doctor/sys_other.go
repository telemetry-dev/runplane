//go:build !linux

package doctor

import (
	"errors"
	"runtime"
)

func clockSynced() (bool, error) {
	return false, errors.New("no kernel clock state on " + runtime.GOOS)
}

func portOwner(int) (pid int, name string) { return 0, "" }
