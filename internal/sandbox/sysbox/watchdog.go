package sysbox

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// watchdog works around sysbox issue #1018: under heavy load sysbox-fs can
// stop answering, and the containers that it serves hang. sysbox-fs has a
// FUSE mount for each container. When requests on its connection wait for
// more than watchdogLimit, the watchdog aborts the connection, so that
// Docker can remove the container.
type watchdog struct {
	connections string // /sys/fs/fuse/connections
	mountinfo   string
	blocked     map[string]time.Time // connection → first sample with waiting requests
}

const (
	watchdogLimit = 60 * time.Second
	sysboxFSDir   = "/var/lib/sysboxfs/" // sysbox-fs mounts at <dir>/<container-id>
)

func newWatchdog() *watchdog {
	return &watchdog{
		connections: "/sys/fs/fuse/connections",
		mountinfo:   "/proc/self/mountinfo",
		blocked:     map[string]time.Time{},
	}
}

// check reads the connections. It aborts each connection that blocks a
// container in ours for more than watchdogLimit, and returns these
// containers.
func (w *watchdog) check(now time.Time, ours func(containerID string) bool) []string {
	conns := w.sysboxConnections()
	var hung []string
	for conn, containerID := range conns {
		if !w.waiting(conn) {
			delete(w.blocked, conn)
			continue
		}
		first, ok := w.blocked[conn]
		if !ok {
			w.blocked[conn] = now
			continue
		}
		if now.Sub(first) <= watchdogLimit || !ours(containerID) {
			continue
		}
		if err := abort(filepath.Join(w.connections, conn, "abort")); err != nil {
			continue
		}
		delete(w.blocked, conn)
		hung = append(hung, containerID)
	}
	for conn := range w.blocked {
		if _, ok := conns[conn]; !ok {
			delete(w.blocked, conn)
		}
	}
	return hung
}

// abort writes 1 to the abort file of a connection, which sysfs has.
func abort(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (w *watchdog) waiting(conn string) bool {
	b, err := os.ReadFile(filepath.Join(w.connections, conn, "waiting"))
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && n > 0
}

// sysboxConnections maps the FUSE connection of each sysbox-fs mount to its
// container. A connection is named by the device number of the mount.
func (w *watchdog) sysboxConnections() map[string]string {
	f, err := os.Open(w.mountinfo)
	if err != nil {
		return nil
	}
	defer f.Close()
	conns := map[string]string{}
	s := bufio.NewScanner(f)
	s.Buffer(nil, 1<<20)
	for s.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		fields := strings.Fields(s.Text())
		sep := slices.Index(fields, "-")
		if len(fields) < 5 || sep < 0 || sep+1 >= len(fields) || !strings.HasPrefix(fields[sep+1], "fuse") {
			continue
		}
		rest, ok := strings.CutPrefix(fields[4], sysboxFSDir)
		if !ok {
			continue
		}
		containerID, _, _ := strings.Cut(rest, "/")
		major, minor, _ := strings.Cut(fields[2], ":")
		ma, err1 := strconv.ParseUint(major, 10, 32)
		mi, err2 := strconv.ParseUint(minor, 10, 32)
		if containerID == "" || err1 != nil || err2 != nil {
			continue
		}
		conns[strconv.FormatUint(ma<<20|mi, 10)] = containerID
	}
	return conns
}

// checkSysboxFS removes the sandboxes that the watchdog finds hung. The
// scheduler then starts new ones.
func (d *Driver) checkSysboxFS() {
	ours := func(containerID string) bool { return d.byContainer(containerID) != nil }
	for _, containerID := range d.dog.check(time.Now(), ours) {
		b := d.byContainer(containerID)
		if b == nil {
			continue
		}
		d.log.Error("sysbox-fs stopped answering, removing the sandbox (sysbox issue #1018)", "sandbox", b.id)
		d.mu.Lock()
		b.failure = "sysbox-fs stopped answering (sysbox issue #1018)"
		d.mu.Unlock()
		go d.remove(b)
	}
}
