package sysbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestWatchdogAbortsBlockedSysboxConnection(t *testing.T) {
	dir := t.TempDir()
	conns := filepath.Join(dir, "connections")
	setWaiting := func(conn, n string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(conns, conn), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(conns, conn, "waiting"), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(conns, conn, "abort"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aborted := func(conn string) bool {
		b, _ := os.ReadFile(filepath.Join(conns, conn, "abort"))
		return string(b) == "1"
	}
	// 52: sysbox-fs of a runplane sandbox. 53: sysbox-fs of another
	// container. 54: a FUSE mount that is not sysbox-fs.
	mountinfo := filepath.Join(dir, "mountinfo")
	os.WriteFile(mountinfo, []byte(
		"30 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+
			"40 30 0:52 / /var/lib/sysboxfs/aaa111/proc/sys rw - fuse sysboxfs rw,user_id=0\n"+
			"41 30 0:52 / /var/lib/sysboxfs/aaa111/sys/devices rw - fuse sysboxfs rw,user_id=0\n"+
			"42 30 0:53 / /var/lib/sysboxfs/bbb222/proc/sys rw - fuse sysboxfs rw,user_id=0\n"+
			"43 30 0:54 / /home/me/remote rw - fuse.sshfs me@host: rw\n"), 0o644)
	setWaiting("52", "3")
	setWaiting("53", "3")
	setWaiting("54", "3")
	w := &watchdog{connections: conns, mountinfo: mountinfo, blocked: map[string]time.Time{}}
	ours := func(id string) bool { return id == "aaa111" }

	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for _, s := range []int{0, 10, 20, 30, 40, 50, 60} {
		if hung := w.check(t0.Add(time.Duration(s)*time.Second), ours); len(hung) > 0 {
			t.Fatalf("after %d s: hung = %v, want none before 60 s", s, hung)
		}
	}
	hung := w.check(t0.Add(70*time.Second), ours)
	if !slices.Equal(hung, []string{"aaa111"}) || !aborted("52") {
		t.Fatalf("after 70 s: hung = %v, abort written = %v; want the runplane sandbox aborted", hung, aborted("52"))
	}
	if aborted("53") || aborted("54") {
		t.Error("the watchdog aborted a connection that is not of a runplane sandbox")
	}

	// A connection that answers in between starts the count again.
	os.WriteFile(filepath.Join(conns, "52", "abort"), nil, 0o600)
	w.check(t0.Add(80*time.Second), ours)
	setWaiting("52", "0")
	w.check(t0.Add(90*time.Second), ours)
	setWaiting("52", "1")
	for _, s := range []int{100, 130, 160} {
		if hung := w.check(t0.Add(time.Duration(s)*time.Second), ours); len(hung) > 0 {
			t.Fatalf("at %d s: hung = %v after the connection answered at 90 s", s, hung)
		}
	}
}
