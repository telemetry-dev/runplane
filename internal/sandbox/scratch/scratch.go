// Package scratch manages the scratch disks of sandboxes: sparse ext4 files
// that take all writes of one job and that runplane removes after the job.
package scratch

import "sync"

// Disks makes scratch disks as copies of one empty ext4 template for each
// size. Keep the template folder on the file system of the disks, so a copy
// can be a reflink.
type Disks struct {
	dir string
	mu  sync.Mutex // one template build at a time
}

func New(templateDir string) *Disks { return &Disks{dir: templateDir} }
