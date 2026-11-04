package firecracker

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox/control"
)

// consoleFIFO in the sandbox folder takes the stdout and stderr of
// Firecracker: the serial console of the guest and the Firecracker log. A
// FIFO, unlike a file, does not grow when a job writes to the console
// without end, and Firecracker only gets write errors while runplane
// restarts.
const consoleFIFO = "console.fifo"

// makeConsole makes the console FIFO. It returns the read end and the write
// end for Firecracker.
func makeConsole(path string) (r, w *os.File, err error) {
	os.Remove(path)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, nil, fmt.Errorf("make console FIFO: %w", err)
	}
	// The read end opens first, so the write end does not wait.
	if r, err = openConsole(path); err != nil {
		return nil, nil, err
	}
	if w, err = os.OpenFile(path, os.O_WRONLY, 0); err != nil {
		r.Close()
		return nil, nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, w, nil
}

// openConsole opens the read end of the console FIFO, also again after a
// restart.
func openConsole(path string) (*os.File, error) {
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open console FIFO: %w", err)
	}
	return r, nil
}

// tailConsole copies the console lines to the sandbox log with the source
// "console" until Firecracker closes its end.
func (v *vm) tailConsole(r *os.File) {
	defer close(v.tailDone)
	control.ReadLines(r, func(line string) {
		v.keepLine(line)
		v.log.Write(control.LogLine{Time: time.Now().UTC(), Source: "console", Text: line})
	})
}
